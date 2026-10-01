package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// tracePath is one path the upstream holds for a prefix in the traceLab.
type tracePath struct {
	gateway  string
	selected bool
}

// traceLab answers the three reads of a fault trace: the chassis SB lists,
// the owner of each chassisredirect port ("" for an unbound one) and the
// paths the upstream router holds per prefix. A test changes it between two
// ticks through set.
type traceLab struct {
	mu         sync.Mutex
	chassis    []string
	owners     map[string]string
	paths      map[string][]tracePath
	chassisErr error
}

func (f *traceLab) set(change func(f *traceLab)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *traceLab) respond(argv []string) (string, error) {
	line := strings.Join(argv, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(line, "--columns=_uuid,name list Chassis"):
		if f.chassisErr != nil {
			return "", f.chassisErr
		}
		rows := [][]any{}
		for _, name := range f.chassis {
			rows = append(rows, []any{ovsUUID("ch-" + name), name})
		}
		return ovsTable([]string{"_uuid", "name"}, rows), nil
	case strings.Contains(line, "find Port_Binding type=chassisredirect"):
		rows := [][]any{}
		for _, port := range sortedKeys(f.owners) {
			chassis := any(ovsSet())
			if owner := f.owners[port]; owner != "" {
				chassis = ovsUUID("ch-" + owner)
			}
			rows = append(rows, []any{port, chassis})
		}
		return ovsTable([]string{"logical_port", "chassis"}, rows), nil
	case strings.Contains(line, "show bgp ipv4 unicast json"):
		routes := map[string][]map[string]any{}
		for prefix, paths := range f.paths {
			for _, p := range paths {
				link, _ := linkFor(p.gateway)
				routes[prefix] = append(routes[prefix], map[string]any{
					"bestpath": p.selected,
					"nexthops": []map[string]any{{"ip": addrOf(link.gatewayCIDR)}},
				})
			}
		}
		b, err := json.Marshal(map[string]any{"routes": routes})
		return string(b), err
	}
	return healthyLabResponses(argv)
}

// newTraceLab is the lab before a double failover on gateway-1 and
// gateway-2: gateway-2 owns lr0's port and carries its two FIPs, gateway-3
// owns lr1's port.
func newTraceLab() *traceLab {
	return &traceLab{
		chassis: gatewayNames(),
		owners:  map[string]string{"cr-lr0-public": "gateway-2", "cr-lr1-public": "gateway-3"},
		paths: map[string][]tracePath{
			"192.0.2.10/32": {{"gateway-2", true}},
			"192.0.2.12/32": {{"gateway-2", true}},
		},
	}
}

// traceEngine builds an engine on the fake clock whose fault trace is paced
// by the returned stub.
func traceEngine(t *testing.T, respond func(argv []string) (string, error)) (*engine, *fakeCommander, *bytes.Buffer, *followStub) {
	t.Helper()
	cmd := &fakeCommander{respond: respond}
	clock := newFakeClock()
	buf := &bytes.Buffer{}
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), nil, greenProbes{},
		newJournal(buf, clock.now), &runRecord{ActionsByName: map[string]int{}})
	e.now, e.wait = clock.now, clock.wait
	return e, cmd, buf, stubTraceTick(e)
}

// stubTraceTick paces the fault trace with the channel-backed stub.
func stubTraceTick(e *engine) *followStub {
	s, tick := newTickStub()
	e.traceTick = tick
	return s
}

// tracedDecision is the decision the trace tests run under.
func tracedDecision(traced bool) decision {
	return decision{
		tick:   6,
		action: &action{name: "double-failover", faultTrace: traced},
		target: "gateway-1", peer: "gateway-2",
	}
}

// journaledSince returns the events a journal gained after its first n,
// without their timestamps.
func journaledSince(t *testing.T, buf *bytes.Buffer, n int) []event {
	t.Helper()
	events := eventsIn(t, buf.String())
	for i := range events {
		events[i].TS = ""
	}
	return events[n:]
}

// traceEvent is a cr-owner or upstream-path event of the traced decision.
func traceEvent(name, object, from, to, detail string) event {
	return event{Event: name, Tick: 6, Action: "double-failover", Object: object, From: from, To: to, Detail: detail}
}

// nextReading lets a trace that waits for its tick take one reading, and
// returns when the trace waits for the next one.
func nextReading(t *testing.T, s *followStub) {
	t.Helper()
	s.tick(t)
	s.awaitPoll(t)
}

// The trace journals where every port and prefix stood before the fault, then
// only what changed, with the value it changed from. A port without an owner,
// a prefix without a selected path and an object that left the reading each
// have a value of their own.
func TestFaultTraceJournalsBaselineAndChanges(t *testing.T) {
	fx := newTraceLab()
	e, _, buf, ticks := traceEngine(t, fx.respond)

	stop := e.startFaultTrace(t.Context(), tracedDecision(true))
	defer stop()

	want := []event{
		traceEvent(evCROwner, "cr-lr0-public", "", "gateway-2", traceBaseline),
		traceEvent(evCROwner, "cr-lr1-public", "", "gateway-3", traceBaseline),
		traceEvent(evUpstreamPath, "192.0.2.10", "", "gateway-2", traceBaseline),
		traceEvent(evUpstreamPath, "192.0.2.12", "", "gateway-2", traceBaseline),
	}
	if got := journaledSince(t, buf, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("the baseline was journaled as\n%+v\nwant\n%+v", got, want)
	}
	ticks.awaitPoll(t)

	steps := []struct {
		name   string
		change func(f *traceLab)
		want   []event
	}{
		{
			name:   "the owner dies and nobody claims the port",
			change: func(f *traceLab) { f.owners["cr-lr0-public"] = "" },
			want:   []event{traceEvent(evCROwner, "cr-lr0-public", "gateway-2", traceUnbound, "")},
		},
		{
			name: "gateway-3 claims the port and the upstream follows",
			change: func(f *traceLab) {
				f.owners["cr-lr0-public"] = "gateway-3"
				f.paths["192.0.2.10/32"] = []tracePath{{"gateway-2", false}, {"gateway-3", true}}
			},
			want: []event{
				traceEvent(evCROwner, "cr-lr0-public", traceUnbound, "gateway-3", ""),
				traceEvent(evUpstreamPath, "192.0.2.10", "gateway-2", "gateway-3", ""),
			},
		},
		{
			name: "a port leaves SB and a prefix keeps only an unselected path",
			change: func(f *traceLab) {
				delete(f.owners, "cr-lr1-public")
				f.paths["192.0.2.12/32"] = []tracePath{{"gateway-2", false}}
			},
			want: []event{
				traceEvent(evCROwner, "cr-lr1-public", "gateway-3", traceAbsent, ""),
				traceEvent(evUpstreamPath, "192.0.2.12", "gateway-2", traceNone, ""),
			},
		},
		{
			name: "the port comes back and a prefix is announced for the first time",
			change: func(f *traceLab) {
				f.owners["cr-lr1-public"] = "gateway-1"
				f.paths["192.0.2.14/32"] = []tracePath{{"gateway-1", true}, {"gateway-3", true}}
			},
			want: []event{
				traceEvent(evCROwner, "cr-lr1-public", traceAbsent, "gateway-1", ""),
				traceEvent(evUpstreamPath, "192.0.2.14", traceAbsent, "gateway-1,gateway-3", ""),
			},
		},
	}
	for _, step := range steps {
		journaled := len(eventsIn(t, buf.String()))
		fx.set(step.change)
		nextReading(t, ticks)

		if got := journaledSince(t, buf, journaled); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s: journaled\n%+v\nwant\n%+v", step.name, got, step.want)
		}
	}
}

// A lab that does not change is journaled once, at the baseline.
func TestFaultTraceIsSilentWithoutChanges(t *testing.T) {
	e, cmd, buf, ticks := traceEngine(t, newTraceLab().respond)

	stop := e.startFaultTrace(t.Context(), tracedDecision(true))
	defer stop()
	baseline, reads := buf.String(), len(cmd.lines())

	ticks.awaitPoll(t)
	for range 3 {
		nextReading(t, ticks)
	}

	if buf.String() != baseline {
		t.Fatalf("three readings of an unchanged lab were journaled:\n%s", buf.String())
	}
	if len(cmd.lines()) == reads {
		t.Fatal("three ticks read nothing from the lab")
	}
}

// A lab without a chassisredirect port and without an announced prefix has
// nothing to trace: the empty reading is the baseline, and it journals
// nothing.
func TestFaultTraceEmptyLab(t *testing.T) {
	e, _, buf, ticks := traceEngine(t, healthyLabResponses)

	stop := e.startFaultTrace(t.Context(), tracedDecision(true))
	defer stop()
	ticks.awaitPoll(t)
	nextReading(t, ticks)

	if buf.Len() != 0 {
		t.Fatalf("a trace of an empty lab was journaled:\n%s", buf.String())
	}
}

// Only a traced action is followed. For any other the engine reads nothing,
// journals nothing and starts no goroutine.
func TestFaultTraceNotStartedForUntracedActions(t *testing.T) {
	e, cmd, buf, ticks := traceEngine(t, newTraceLab().respond)
	issued := len(cmd.lines())

	stop := e.startFaultTrace(t.Context(), tracedDecision(false))
	stop()

	if got := len(cmd.lines()); got != issued {
		t.Fatalf("an untraced action issued %d commands: %v", got-issued, cmd.lines()[issued:])
	}
	if buf.Len() != 0 {
		t.Fatalf("an untraced action was journaled:\n%s", buf.String())
	}
	if got := ticks.stopped.Load(); got != 0 {
		t.Fatalf("an untraced action started %d trace goroutines", got)
	}
}

// A read that keeps failing is journaled once, not once a second, and again
// only after a read succeeded in between. The values known before the failure
// stay the trace's reference, so the recovery journals no change. A reader
// whose first read fails takes its baseline from the first one that succeeds.
func TestFaultTraceJournalsAReadErrorOnce(t *testing.T) {
	checkErrors := func(t *testing.T, events []event) []event {
		t.Helper()
		var out []event
		for _, ev := range events {
			if ev.Event != evCheckError {
				continue
			}
			if ev.Tick != 6 || ev.Action != "double-failover" ||
				!strings.HasPrefix(ev.Detail, "fault trace: ") ||
				!strings.Contains(ev.Detail, "read ovsdb table Chassis") {
				t.Fatalf("the failed read was journaled as %+v", ev)
			}
			out = append(out, ev)
		}
		return out
	}

	t.Run("after the baseline", func(t *testing.T) {
		fx := newTraceLab()
		e, _, buf, ticks := traceEngine(t, fx.respond)

		stop := e.startFaultTrace(t.Context(), tracedDecision(true))
		defer stop()
		baseline := len(eventsIn(t, buf.String()))
		ticks.awaitPoll(t)

		fx.set(func(f *traceLab) { f.chassisErr = errBoom })
		for range 3 {
			nextReading(t, ticks)
		}
		got := journaledSince(t, buf, baseline)
		if len(got) != 1 || len(checkErrors(t, got)) != 1 {
			t.Fatalf("three failed readings journaled %+v, want one %s", got, evCheckError)
		}

		fx.set(func(f *traceLab) { f.chassisErr = nil })
		nextReading(t, ticks)
		if got := journaledSince(t, buf, baseline); len(got) != 1 {
			t.Fatalf("the recovered read journaled the owners it already knew: %+v", got[1:])
		}

		fx.set(func(f *traceLab) { f.chassisErr = errBoom })
		nextReading(t, ticks)
		got = journaledSince(t, buf, baseline)
		if len(got) != 2 || len(checkErrors(t, got)) != 2 {
			t.Fatalf("a failure after a successful read journaled %+v, want a second %s", got, evCheckError)
		}
	})

	t.Run("before the baseline", func(t *testing.T) {
		fx := newTraceLab()
		fx.chassisErr = errBoom
		e, _, buf, ticks := traceEngine(t, fx.respond)

		stop := e.startFaultTrace(t.Context(), tracedDecision(true))
		defer stop()
		got := journaledSince(t, buf, 0)
		if len(checkErrors(t, got)) != 1 {
			t.Fatalf("a failed first read journaled %+v, want one %s", got, evCheckError)
		}
		for _, ev := range got {
			if ev.Event == evCROwner {
				t.Fatalf("a failed first read journaled an owner: %+v", ev)
			}
		}
		journaled := len(got)
		ticks.awaitPoll(t)

		fx.set(func(f *traceLab) { f.chassisErr = nil })
		nextReading(t, ticks)

		want := []event{
			traceEvent(evCROwner, "cr-lr0-public", "", "gateway-2", traceBaseline),
			traceEvent(evCROwner, "cr-lr1-public", "", "gateway-3", traceBaseline),
		}
		if got := journaledSince(t, buf, journaled); !reflect.DeepEqual(got, want) {
			t.Fatalf("the first successful read journaled\n%+v\nwant the baseline\n%+v", got, want)
		}
	})
}

// A read that fails because the trace was stopped is not a lab failure: it is
// not journaled.
func TestFaultTraceIgnoresAReadFailedByItsOwnStop(t *testing.T) {
	e, _, buf, _ := traceEngine(t, healthyLabResponses)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &traceReader{event: evCROwner, read: func(context.Context, *lab) (map[string]string, error) {
		return nil, errBoom
	}}

	e.traceReading(ctx, tracedDecision(true), r)

	if buf.Len() != 0 {
		t.Fatalf("a read failed by the trace's own stop was journaled:\n%s", buf.String())
	}
}

// execute takes the trace's baseline before it journals the inject, so the
// baseline is a reading of the lab before the fault. It stops the trace and
// waits for its goroutine on every way out, so no reading is journaled after
// the action is over: neither after a failed inject nor after a convergence.
func TestExecuteJoinsTheFaultTrace(t *testing.T) {
	tests := []struct {
		name      string
		inject    error
		wantEvent string
	}{
		{name: "the inject fails", inject: errBoom, wantEvent: evRunAborted},
		{name: "the action converges", wantEvent: evConverged},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, _, buf := followEngine(t, newTraceLab().respond)
			stubFollowTick(e)
			ticks := stubTraceTick(e)

			act := noopActions("double-failover")[0]
			act.faultTrace = true
			act.inject = func(context.Context, *lab, string, int) error { return tc.inject }

			e.execute(t.Context(), decision{tick: 1, action: act, target: "gateway-1", hold: 5 * time.Second})

			baseline, inject := -1, -1
			for i, ev := range eventsIn(t, buf.String()) {
				switch {
				case ev.Event == evCROwner && ev.Detail == traceBaseline && baseline < 0:
					baseline = i
				case ev.Event == evInject:
					inject = i
				}
			}
			if baseline < 0 || inject < 0 || baseline > inject {
				t.Fatalf("baseline at %d, inject at %d: the baseline must be journaled before the inject:\n%s",
					baseline, inject, buf.String())
			}
			if got := ticks.stopped.Load(); got != 1 {
				t.Fatalf("execute returned with %d traces stopped, want the one it started", got)
			}
			if ev := lastEventOf(t, buf.String(), tc.wantEvent); ev.Event != tc.wantEvent {
				t.Fatalf("the action did not end in %s:\n%s", tc.wantEvent, buf.String())
			}
		})
	}
}
