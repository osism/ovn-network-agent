package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runEngine drives a full engine run against the fakes and returns the
// journal it wrote plus the run record it filled in.
func runEngine(t *testing.T, seed int64, duration time.Duration,
	actions []*action, mutate func(*engine)) (string, *runRecord) {
	t.Helper()

	clock := newFakeClock()
	cmd := &fakeCommander{respond: healthyLabResponses}
	var buf bytes.Buffer
	jrnl := newJournal(&buf, clock.now)

	rec := &runRecord{
		Inputs: runInputs{
			Seed:       seed,
			DurationMS: duration.Milliseconds(),
			TickMinMS:  (10 * time.Second).Milliseconds(),
			TickMaxMS:  (30 * time.Second).Milliseconds(),
			Lab:        "ovn-e2e",
		},
		ActionsByName: map[string]int{},
	}
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), actions, greenProbes{}, jrnl, rec)
	e.wait = clock.wait
	e.now = clock.now
	if mutate != nil {
		mutate(e)
	}
	e.run(context.Background())
	rec.finalize(clock.now())
	return buf.String(), rec
}

func TestSameSeedReplaysIdenticalSequence(t *testing.T) {
	first, firstRec := runEngine(t, 42, 20*time.Minute,
		noopActions("controller-restart", "gateway-kill", "agent-terminate"), nil)
	second, secondRec := runEngine(t, 42, 20*time.Minute,
		noopActions("controller-restart", "gateway-kill", "agent-terminate"), nil)

	a, b := decisionsIn(t, first), decisionsIn(t, second)
	if len(a) == 0 {
		t.Fatal("the run made no decisions at all")
	}
	if len(a) != len(b) {
		t.Fatalf("same inputs produced %d and %d decisions", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("decision %d diverged: %q vs %q", i+1, a[i], b[i])
		}
	}
	if firstRec.Decisions != secondRec.Decisions {
		t.Fatalf("decision counts diverged: %+v vs %+v", firstRec.Decisions, secondRec.Decisions)
	}
}

func TestDifferentSeedsDiverge(t *testing.T) {
	first, _ := runEngine(t, 42, 20*time.Minute,
		noopActions("controller-restart", "gateway-kill", "agent-terminate"), nil)
	second, _ := runEngine(t, 43, 20*time.Minute,
		noopActions("controller-restart", "gateway-kill", "agent-terminate"), nil)

	a, b := decisionsIn(t, first), decisionsIn(t, second)
	if len(a) == len(b) && slicesEqual(a, b) {
		t.Fatal("seeds 42 and 43 produced the same decision sequence")
	}
}

// A guardrail skip must not consume a different number of random values
// than an execution, or a replay against a lab that behaved differently
// would diverge from the first decision the guardrail blocked onwards —
// and the journal would be useless for triage.
func TestGuardrailSkipDoesNotShiftDraws(t *testing.T) {
	healthy, _ := runEngine(t, 42, 20*time.Minute,
		noopActions("controller-restart", "gateway-kill"), nil)
	// gateway-2 never came back from an earlier fault: every decision
	// that targets it is skipped.
	parked, parkedRec := runEngine(t, 42, 20*time.Minute,
		noopActions("controller-restart", "gateway-kill"), func(e *engine) {
			e.nodes["gateway-2"] = nodeUnconverged
		})

	a, b := decisionsIn(t, healthy), decisionsIn(t, parked)
	if len(b) < len(a) {
		t.Fatalf("the run with skips fit fewer decisions (%d) than the all-healthy run (%d)", len(b), len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("decision %d diverged after a guardrail skip: %q vs %q", i+1, a[i], b[i])
		}
	}
	if parkedRec.Decisions.Skipped == 0 {
		t.Fatal("no decision was skipped even though gateway-2 was parked")
	}
	var skipped bool
	for _, e := range eventsIn(t, parked) {
		if e.Event == evDecision && e.SkipReason == skipTargetNotHealthy && e.Target == "gateway-2" {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("no decision was journaled with skip_reason=%s for gateway-2", skipTargetNotHealthy)
	}
}

func TestWeightedPickNeverDrawsAZeroWeightAction(t *testing.T) {
	actions := noopActions("controller-restart", "gateway-kill", "agent-terminate")
	applyWeights(actions, map[string]int{"gateway-kill": 0, "controller-restart": 3})

	_, rec := runEngine(t, 42, 30*time.Minute, actions, nil)

	if rec.ActionsByName["gateway-kill"] != 0 {
		t.Fatalf("a zero-weight action was drawn %d times", rec.ActionsByName["gateway-kill"])
	}
	if rec.ActionsByName["controller-restart"] == 0 {
		t.Fatal("the highest-weighted action was never drawn")
	}
	if rec.ActionsByName["agent-terminate"] == 0 {
		t.Fatal("a positively-weighted action was never drawn")
	}
}

// With no action carrying weight the engine degrades to a probe-and-
// check-only run: it still ticks, but injects nothing.
func TestZeroWeightRegistryInjectsNothing(t *testing.T) {
	actions := noopActions("controller-restart", "gateway-kill")
	applyWeights(actions, map[string]int{"controller-restart": 0, "gateway-kill": 0})

	journal, rec := runEngine(t, 42, 5*time.Minute, actions, nil)

	if rec.Decisions.Executed != 0 {
		t.Fatalf("executed %d actions with an all-zero-weight registry", rec.Decisions.Executed)
	}
	if rec.Decisions.Skipped == 0 {
		t.Fatal("the run made no decisions at all")
	}
	for _, e := range eventsIn(t, journal) {
		if e.Event == evDecision && e.SkipReason != skipNoWeightedAction {
			t.Fatalf("decision skipped for %q, want %q", e.SkipReason, skipNoWeightedAction)
		}
	}
}

func TestGuardrailsBlockUnhealthyTargetAndLastGateway(t *testing.T) {
	gatewayAct := noopActions("controller-restart")[0]
	centralAct := &action{name: "nb-pause", scope: scopeCentral, weight: 1}
	pairAct := &action{name: "double-failover", scope: scopeGatewayPair, weight: 1}
	driftAct := &action{name: "kernel-route-drop", scope: scopeGateway, weight: 1,
		applicable: func(context.Context, string, int) bool { return false }}

	tests := []struct {
		name   string
		action *action
		target string
		peer   string
		nodes  map[string]string
		want   string
	}{
		{
			name:   "all nodes healthy",
			action: gatewayAct,
			target: "gateway-1",
			nodes:  map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:   "",
		},
		{
			name:   "target still converging from an earlier fault",
			action: gatewayAct,
			target: "gateway-1",
			nodes:  map[string]string{"gateway-1": nodeConverging, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:   skipTargetNotHealthy,
		},
		{
			name:   "target never came back",
			action: gatewayAct,
			target: "gateway-1",
			nodes:  map[string]string{"gateway-1": nodeUnconverged, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:   skipTargetNotHealthy,
		},
		{
			// A drift-style fault whose object the target does not carry is a
			// journaled skip, not a no-op deletion — even with every gateway
			// healthy and a peer to fail over to.
			name:   "drift action skipped when its object is absent",
			action: driftAct,
			target: "gateway-1",
			nodes:  map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:   skipNotApplicable,
		},
		{
			name:   "target is the last healthy gateway",
			action: gatewayAct,
			target: "gateway-1",
			nodes:  map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeUnconverged, "gateway-3": nodeUnconverged},
			want:   skipNoHealthyPeer,
		},
		{
			// A central-scoped fault does not depend on how many gateways are
			// up: pausing the database with only one healthy gateway is fine.
			name:   "central action needs no healthy gateway peer",
			action: centralAct,
			target: centralNode,
			nodes: map[string]string{
				centralNode: nodeHealthy,
				"gateway-1": nodeHealthy, "gateway-2": nodeUnconverged, "gateway-3": nodeUnconverged,
			},
			want: "",
		},
		{
			name:   "central action skipped when central has not converged",
			action: centralAct,
			target: centralNode,
			nodes: map[string]string{
				centralNode: nodeConverging,
				"gateway-1": nodeHealthy, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy,
			},
			want: skipTargetNotHealthy,
		},
		{
			name:   "pair action skipped when the ring-next peer is unhealthy",
			action: pairAct,
			target: "gateway-1",
			peer:   "gateway-2",
			nodes:  map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeConverging, "gateway-3": nodeHealthy},
			want:   skipPeerNotHealthy,
		},
		{
			// A pair holds two gateways down, so it needs a third to fail
			// over to.
			name:   "pair action skipped when no third gateway is healthy",
			action: pairAct,
			target: "gateway-1",
			peer:   "gateway-2",
			nodes:  map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeHealthy, "gateway-3": nodeUnconverged},
			want:   skipNoHealthyPeer,
		},
		{
			name:   "pair action runs with target, peer and a third gateway healthy",
			action: pairAct,
			target: "gateway-1",
			peer:   "gateway-2",
			nodes:  map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newFakeClock()
			e := newEngine(newTestLab(&fakeCommander{}, clock), defaultTestProfile(t), []*action{tc.action},
				greenProbes{}, newJournal(&bytes.Buffer{}, clock.now),
				&runRecord{ActionsByName: map[string]int{}})
			e.nodes = tc.nodes

			got := e.guardrails(context.Background(),
				decision{action: tc.action, target: tc.target, peer: tc.peer})
			if got != tc.want {
				t.Fatalf("guardrails = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunStopsAtDuration(t *testing.T) {
	actions := noopActions("controller-restart")
	// Pin the tick interval so the number of decisions that fit is exact:
	// ticks land at 10s, 20s and 30s, and the tick that would land at 40s
	// is past the 35s deadline.
	clock := newFakeClock()
	var buf bytes.Buffer
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       42,
			DurationMS: (35 * time.Second).Milliseconds(),
			TickMinMS:  (10 * time.Second).Milliseconds(),
			TickMaxMS:  (10 * time.Second).Milliseconds(),
		},
		ActionsByName: map[string]int{},
	}
	e := newEngine(newTestLab(&fakeCommander{respond: healthyLabResponses}, clock),
		defaultTestProfile(t), actions, greenProbes{}, newJournal(&buf, clock.now), rec)
	e.wait, e.now = clock.wait, clock.now
	// The action holds for 0s so only the tick interval advances the clock.
	actions[0].holdMin, actions[0].holdMax = 0, 0

	e.run(context.Background())
	rec.finalize(clock.now())

	if got := len(decisionsIn(t, buf.String())); got != 3 {
		t.Fatalf("made %d decisions in a 35s run at a 10s tick, want 3", got)
	}
	if rec.Result != resultPass {
		t.Fatalf("result = %q, want %q — nothing went wrong", rec.Result, resultPass)
	}

	// The per-action recovery durations are what the run exists to
	// produce: a passing run that shipped an empty `recoveries` would be
	// indistinguishable from one that measured nothing.
	if rec.Decisions.Executed == 0 {
		t.Fatal("the run executed no action at all")
	}
	if len(rec.Recoveries) != rec.Decisions.Executed {
		t.Fatalf("recorded %d recoveries for %d executed actions",
			len(rec.Recoveries), rec.Decisions.Executed)
	}
	for _, r := range rec.Recoveries {
		if r.Action != actions[0].name {
			t.Fatalf("recovery %+v names an action the run never executed", r)
		}
		if e.nodeState(r.Target) != nodeHealthy {
			t.Fatalf("recovery recorded for %s, which is not back in service", r.Target)
		}
		if r.BudgetMS != actions[0].recoveryBudget.Milliseconds() {
			t.Fatalf("recovery budget = %d ms, want the action's %s",
				r.BudgetMS, actions[0].recoveryBudget)
		}
		if r.ConvergedMS < 0 || r.ConvergedMS > r.BudgetMS {
			t.Fatalf("converged in %d ms, outside the %d ms budget it was gated on",
				r.ConvergedMS, r.BudgetMS)
		}
	}
	var converged int
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event == evConverged {
			converged++
		}
	}
	if converged != rec.Decisions.Executed {
		t.Fatalf("journaled %d %s events for %d executed actions",
			converged, evConverged, rec.Decisions.Executed)
	}
}

// A run cancelled mid-hold must still undo the fault it is holding.
// Otherwise the lab is left with a SIGKILLed gateway whose restart policy
// is pinned to "no" and whose containerlab veth is gone — and every
// scenario after it runs against a broken lab.
func TestCancelledRunRestoresTheFaultItIsHolding(t *testing.T) {
	clock := newFakeClock()
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       42,
			DurationMS: time.Minute.Milliseconds(),
			TickMinMS:  (10 * time.Second).Milliseconds(),
			TickMaxMS:  (10 * time.Second).Milliseconds(),
		},
		ActionsByName: map[string]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())

	actions := noopActions("gateway-kill")
	// The operator hits Ctrl-C while the fault is held.
	actions[0].inject = func(context.Context, *lab, string, int) error {
		cancel()
		return nil
	}
	var restores int
	var restoreCtxErr error
	actions[0].restore = func(rctx context.Context, _ *lab, _ string) error {
		restores++
		restoreCtxErr = rctx.Err()
		return nil
	}

	e := newEngine(newTestLab(&fakeCommander{respond: healthyLabResponses}, clock),
		defaultTestProfile(t), actions, greenProbes{}, newJournal(&bytes.Buffer{}, clock.now), rec)
	e.wait, e.now = clock.wait, clock.now

	e.run(ctx)

	if restores != 1 {
		t.Fatalf("the held fault was restored %d times, want exactly once", restores)
	}
	// A restore handed the cancelled context would have every docker
	// command killed on sight, so it must run on a context of its own.
	if restoreCtxErr != nil {
		t.Fatalf("the restore ran on the cancelled context (%v)", restoreCtxErr)
	}
}

// The signal can just as well land while the restore is already running,
// and a restore is long enough for that to be likely: a gateway-kill
// restore starts the container, waits out two daemon bring-ups and pushes
// BGP, which is minutes. Cancelled half-way through it leaves the same
// wreckage as one that never ran, so the restore must not be riding on
// the run's context at all.
func TestCancelDuringTheRestoreDoesNotKillIt(t *testing.T) {
	clock := newFakeClock()
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       42,
			DurationMS: time.Minute.Milliseconds(),
			TickMinMS:  (10 * time.Second).Milliseconds(),
			TickMaxMS:  (10 * time.Second).Milliseconds(),
		},
		ActionsByName: map[string]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())

	actions := noopActions("gateway-kill")
	var restoreCtxErr error
	var bounded bool
	actions[0].restore = func(rctx context.Context, _ *lab, _ string) error {
		// The operator hits Ctrl-C while the restore is in flight.
		cancel()
		restoreCtxErr = rctx.Err()
		_, bounded = rctx.Deadline()
		return nil
	}

	e := newEngine(newTestLab(&fakeCommander{respond: healthyLabResponses}, clock),
		defaultTestProfile(t), actions, greenProbes{}, newJournal(&bytes.Buffer{}, clock.now), rec)
	e.wait, e.now = clock.wait, clock.now

	e.run(ctx)

	if restoreCtxErr != nil {
		t.Fatalf("the signal killed the restore that was already running (%v)", restoreCtxErr)
	}
	// Detached from the signal, but not unbounded: a restore that hangs
	// would keep the runner alive until CI's job timeout.
	if !bounded {
		t.Fatal("the restore ran on a context with no deadline of its own")
	}
}

// An action the runner cannot undo is worse than one it cannot inject:
// the lab is left genuinely broken. The node is parked and the run is
// stamped a harness fault — the failing command was the runner's own.
func TestFailedRestoreParksTheNodeAndFaultsTheHarness(t *testing.T) {
	actions := noopActions("controller-restart")
	actions[0].restore = func(context.Context, *lab, string) error {
		return errBoom
	}

	journal, rec := runEngine(t, 42, 5*time.Minute, actions, nil)

	if rec.Result != resultHarnessFault {
		t.Fatalf("result = %q, want %q", rec.Result, resultHarnessFault)
	}
	if len(rec.Violations) == 0 || rec.Violations[0].Kind != violationActionFailed {
		t.Fatalf("violations = %+v, want a %s", rec.Violations, violationActionFailed)
	}
	if !strings.Contains(rec.Violations[0].Detail, "restore") {
		t.Fatalf("violation detail %q does not name the phase that failed", rec.Violations[0].Detail)
	}
	if !parkedIn(t, journal, rec.Violations[0].Target) {
		t.Fatalf("%s was not parked after a fault the runner could not undo",
			rec.Violations[0].Target)
	}
}

// converged() has four gates. The container-health one is driven by
// TestRecoveryBudgetExpiryIsAViolation; these are the other three. Each is
// the only thing between a node that came back wrong — no agent process,
// a chassis that never re-registered, a data path that never came back —
// and being declared healthy and re-targeted.
func TestConvergenceGatesOnTheAgentTheChassisAndTheDataPath(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*engine)
	}{
		{
			// The gwnode entrypoint brings OVS, ovn-controller and FRR up
			// before it execs the agent, so a container can answer "healthy"
			// with its chassis row still in SB while the node has no agent at
			// all. Re-admitting it there is what filled the nightly runs with
			// `agent-down` violations against nodes that were merely booting.
			name: "the node is up but its agent has not been exec'd yet",
			mutate: func(e *engine) {
				e.lab.cmd = &fakeCommander{respond: func(argv []string) (string, error) {
					if strings.Contains(strings.Join(argv, " "), "pgrep -f "+agentBinary) {
						return "", errExit(t, 1)
					}
					return healthyLabResponses(argv)
				}}
			},
		},
		{
			name: "the node is up but its chassis never re-registers in SB",
			mutate: func(e *engine) {
				e.lab.cmd = &fakeCommander{respond: func(argv []string) (string, error) {
					if strings.Contains(strings.Join(argv, " "), "find Chassis name=") {
						return "\n", nil
					}
					return healthyLabResponses(argv)
				}}
			},
		},
		{
			name:   "the node is up but its data path stays red",
			mutate: func(e *engine) { e.probes = redProbes{} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			journal, rec := runEngine(t, 42, 5*time.Minute, noopActions("gateway-kill"), tc.mutate)

			if len(rec.Violations) == 0 || rec.Violations[0].Kind != violationRecoveryTimeout {
				t.Fatalf("violations = %+v, want a %s", rec.Violations, violationRecoveryTimeout)
			}
			if rec.Result != resultFail {
				t.Fatalf("result = %q, want %q", rec.Result, resultFail)
			}
			if !parkedIn(t, journal, rec.Violations[0].Target) {
				t.Fatalf("%s was declared converged and left in service",
					rec.Violations[0].Target)
			}
		})
	}
}

// A parked node is never re-healed, and converged() gates on every probe
// being green — so once one node is parked, no action against any other
// gateway can converge either. Left running, the engine would spend the
// rest of the duration burning recovery budgets on a condition that is
// structurally unsatisfiable, park every remaining gateway, and bury the
// original fault under violations derived from it. It stops at the first
// park instead.
func TestAParkedNodeStopsTheRun(t *testing.T) {
	// The node comes back up, but its data path never does.
	journal, rec := runEngine(t, 42, 20*time.Minute, noopActions("gateway-kill"),
		func(e *engine) { e.probes = redProbes{} })

	if rec.Decisions.Executed != 1 {
		t.Fatalf("executed %d actions, want exactly the one that parked its target: "+
			"every later action can only time out on the same red probes",
			rec.Decisions.Executed)
	}
	if len(rec.Violations) != 1 || rec.Violations[0].Kind != violationRecoveryTimeout {
		t.Fatalf("violations = %+v, want exactly one %s and no violation derived from it",
			rec.Violations, violationRecoveryTimeout)
	}

	// The operator reading the journal must see why the run stopped short
	// of its duration.
	var aborted []event
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evRunAborted {
			aborted = append(aborted, ev)
		}
	}
	if len(aborted) != 1 || aborted[0].Target != rec.Violations[0].Target {
		t.Fatalf("journaled %+v, want one %s naming the parked node %s",
			aborted, evRunAborted, rec.Violations[0].Target)
	}
}

// The VIP's routes are re-plumbed at whichever chassis owns
// cr-lr0-public on every call — the agent does not manage them, and the
// master's scope-link route dies with its container netns, so an owner
// that comes back from a recycle with an unchanged claim needs the routes
// again just as much as a new owner does (run 30272920820). The journal
// still only records actual movement.
func TestFollowMasterRepointsTheVIPWhenTheMasterMoves(t *testing.T) {
	master := "gateway-1"
	var routesFail bool
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case routesFail && strings.Contains(line, "ip route replace"):
			return "", errBoom
		case strings.Contains(line, "--columns=name list Chassis"):
			return master + "\n", nil
		}
		return healthyLabResponses(argv)
	}}
	clock := newFakeClock()
	var buf bytes.Buffer
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), nil, greenProbes{},
		newJournal(&buf, clock.now), &runRecord{ActionsByName: map[string]int{}})
	e.now = clock.now

	e.followMaster(context.Background(), phaseConverge)

	if e.vipOwner != "gateway-1" {
		t.Fatalf("vipOwner = %q after the first master was seen, want gateway-1", e.vipOwner)
	}
	if got := cmd.count("ip route replace"); got != 2 {
		t.Fatalf("issued %d route commands, want the forward route on upstream and "+
			"the scope-link route on the master: %v", got, cmd.lines())
	}

	// The master has not moved: the routes are still re-issued — a
	// recycled owner's netns came back without its scope-link route, and
	// this call is the only thing that puts it back — but nothing is
	// journaled, which is what keeps the journal readable.
	e.followMaster(context.Background(), phaseConverge)

	if got := cmd.count("ip route replace"); got != 4 {
		t.Fatalf("issued %d route commands after an unmoved master, want the re-plumb "+
			"(a recycled owner has no scope-link route): %v", got, cmd.lines())
	}

	// Re-election, and the re-point fails: the owner must stay where the
	// routes actually point, and the failure must be journaled.
	master, routesFail = "gateway-2", true
	e.followMaster(context.Background(), phaseConverge)

	if e.vipOwner != "gateway-1" {
		t.Fatalf("vipOwner = %q after a failed re-point, want the routes' real owner gateway-1", e.vipOwner)
	}

	repoints := repointsIn(t, buf.String())
	if len(repoints) != 2 {
		t.Fatalf("journaled %d %s events, want one per master change: %+v",
			len(repoints), evVIPRepoint, repoints)
	}
	if repoints[0].Target != "gateway-1" || repoints[0].Detail != "" {
		t.Fatalf("the successful re-point was journaled as %+v", repoints[0])
	}
	if repoints[1].Target != "gateway-2" || repoints[1].Detail == "" {
		t.Fatalf("the failed re-point was journaled without a reason: %+v", repoints[1])
	}
	for _, ev := range repoints {
		if ev.Phase != phaseConverge {
			t.Fatalf("a re-point made in the %s phase was journaled as %+v", phaseConverge, ev)
		}
	}
}

// movableMaster answers the lab's queries as healthyLabResponses does,
// except that the chassis owning cr-lr0-public is whichever one the test
// set last. The owner poll reads the name from its own goroutine while the
// test moves it, so mu guards name.
type movableMaster struct {
	mu   sync.Mutex
	name string
}

func (m *movableMaster) set(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.name = name
}

func (m *movableMaster) respond(argv []string) (string, error) {
	if strings.Contains(strings.Join(argv, " "), "--columns=name list Chassis") {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.name + "\n", nil
	}
	return healthyLabResponses(argv)
}

// followEngine builds an engine on the default profile whose VIP routes
// point at gateway-1, the state drive leaves behind at run start. The
// journal it returns opens with that start re-point.
func followEngine(t *testing.T, respond func(argv []string) (string, error)) (*engine, *fakeCommander, *bytes.Buffer) {
	t.Helper()
	cmd := &fakeCommander{respond: respond}
	clock := newFakeClock()
	buf := &bytes.Buffer{}
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), nil, greenProbes{},
		newJournal(buf, clock.now), &runRecord{ActionsByName: map[string]int{}})
	e.now, e.wait = clock.now, clock.wait

	e.followMaster(t.Context(), phaseStart)
	if e.vipOwner != "gateway-1" {
		t.Fatalf("vipOwner = %q after the start re-point, want gateway-1", e.vipOwner)
	}
	return e, cmd, buf
}

// repointsIn extracts the vip-repoint events from a journal, in order.
func repointsIn(t *testing.T, journal string) []event {
	t.Helper()
	var out []event
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evVIPRepoint {
			out = append(out, ev)
		}
	}
	return out
}

// Beside a fault the routes are left alone until the owner moves: the
// chassis SB still names may be the container the fault took down, and a
// re-plumb into it would fail on every poll.
func TestFollowMasterInAFaultPhaseRepointsOnlyOnAnOwnerChange(t *testing.T) {
	for _, phase := range []string{phaseInject, phaseHold} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			master := &movableMaster{name: "gateway-1"}
			e, cmd, buf := followEngine(t, master.respond)
			plumbed, journaled := cmd.count("ip route replace"), buf.String()

			e.followMaster(t.Context(), phase)

			if got := cmd.count("ip route replace") - plumbed; got != 0 {
				t.Fatalf("an unmoved owner was re-plumbed in the %s phase with %d route commands: %v",
					phase, got, cmd.lines())
			}
			if buf.String() != journaled {
				t.Fatalf("an unmoved owner was journaled in the %s phase:\n%s", phase, buf.String())
			}

			master.set("gateway-2")
			e.followMaster(t.Context(), phase)

			if got := cmd.count("ip route replace") - plumbed; got != 2 {
				t.Fatalf("issued %d route commands after the owner moved, want the forward "+
					"route and the scope-link route: %v", got, cmd.lines())
			}
			for _, want := range []string{
				"exec clab-ovn-e2e-upstream ip route replace 192.0.2.50/32 via 100.64.2.2",
				"exec clab-ovn-e2e-gateway-2 ip route replace 192.0.2.50/32 dev br-ex scope link",
			} {
				if cmd.count(want) != 1 {
					t.Fatalf("the re-point at gateway-2 lacks %q: %v", want, cmd.lines())
				}
			}
			if e.vipOwner != "gateway-2" {
				t.Fatalf("vipOwner = %q after the owner moved, want gateway-2", e.vipOwner)
			}
			repoints := repointsIn(t, buf.String())
			if len(repoints) != 2 {
				t.Fatalf("journaled %d %s events, want the start one and the move: %+v",
					len(repoints), evVIPRepoint, repoints)
			}
			if got := repoints[1]; got.Phase != phase || got.Target != "gateway-2" || got.Detail != "" {
				t.Fatalf("the move was journaled as %+v, want phase %s and target gateway-2", got, phase)
			}
		})
	}
}

// An unbound cr-lr0-public (a re-election in flight) and an SB that does
// not answer (an sb-pause hold) both leave the poll without an owner to
// follow. It keeps the routes where they are and journals nothing.
func TestFollowMasterInAFaultPhaseIgnoresAnUnboundPort(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
	}{
		{name: "the port is unbound", out: "\n"},
		{name: "SB does not answer", err: errBoom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ownerless bool
			e, cmd, buf := followEngine(t, func(argv []string) (string, error) {
				if ownerless && strings.Contains(strings.Join(argv, " "), "--columns=chassis find Port_Binding") {
					return tc.out, tc.err
				}
				return healthyLabResponses(argv)
			})
			ownerless = true
			plumbed, journaled := cmd.count("ip route replace"), buf.String()

			e.followMaster(t.Context(), phaseHold)

			if got := cmd.count("ip route replace") - plumbed; got != 0 {
				t.Fatalf("issued %d route commands with no owner to point the routes at: %v",
					got, cmd.lines())
			}
			if buf.String() != journaled {
				t.Fatalf("a poll that found no owner was journaled:\n%s", buf.String())
			}
			if e.vipOwner != "gateway-1" {
				t.Fatalf("vipOwner = %q after a poll that found no owner, want gateway-1", e.vipOwner)
			}
		})
	}
}

// A re-point that fails leaves the owner where the routes still point, so
// the next poll retries it, and every failed attempt is journaled. A poll
// that was stopped mid-command did not fail: its command was killed.
func TestFollowMasterJournalsAFailedRepointUnlessThePollWasStopped(t *testing.T) {
	master := &movableMaster{name: "gateway-1"}
	var routesFail bool
	e, _, buf := followEngine(t, func(argv []string) (string, error) {
		if routesFail && strings.Contains(strings.Join(argv, " "), "ip route replace") {
			return "", errBoom
		}
		return master.respond(argv)
	})
	master.set("gateway-2")
	routesFail = true

	for attempt := 1; attempt <= 2; attempt++ {
		e.followMaster(t.Context(), phaseHold)

		if e.vipOwner != "gateway-1" {
			t.Fatalf("vipOwner = %q after failed re-point %d, want the routes' real owner gateway-1",
				e.vipOwner, attempt)
		}
		repoints := repointsIn(t, buf.String())
		if len(repoints) != 1+attempt {
			t.Fatalf("journaled %d %s events after failed re-point %d, want the start one "+
				"and one per attempt: %+v", len(repoints), evVIPRepoint, attempt, repoints)
		}
		failed := repoints[attempt]
		if failed.Phase != phaseHold || failed.Target != "gateway-2" ||
			!strings.Contains(failed.Detail, "point 192.0.2.50 at gateway-2 on upstream: boom") {
			t.Fatalf("failed re-point %d was journaled as %+v", attempt, failed)
		}
	}

	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	journaled := buf.String()
	e.followMaster(stopped, phaseHold)

	if buf.String() != journaled {
		t.Fatalf("a re-point killed by the poll's stop was journaled as a failure:\n%s", buf.String())
	}
	if e.vipOwner != "gateway-1" {
		t.Fatalf("vipOwner = %q after a re-point that was killed, want gateway-1", e.vipOwner)
	}
}

// followStub is the channel-backed followTick the tests drive the owner
// poll with, so none of them sleeps. stopped counts the polls that ended
// because their context was cancelled.
type followStub struct {
	entered chan struct{}
	ticks   chan struct{}
	stopped atomic.Int32
}

// newTickStub returns the channel-backed stub and the tick function that
// paces a goroutine with it.
func newTickStub() (*followStub, func(ctx context.Context) bool) {
	s := &followStub{entered: make(chan struct{}), ticks: make(chan struct{})}
	return s, func(ctx context.Context) bool {
		select { // announces "waiting for the next tick": the previous poll is done
		case s.entered <- struct{}{}:
		case <-ctx.Done():
			s.stopped.Add(1)
			return false
		}
		select {
		case <-s.ticks:
			return true
		case <-ctx.Done():
			s.stopped.Add(1)
			return false
		}
	}
}

func stubFollowTick(e *engine) *followStub {
	s, tick := newTickStub()
	e.followTick = tick
	return s
}

// awaitPoll blocks until the poll waits for its next tick, which is also
// when the poll the previous tick started has finished.
func (s *followStub) awaitPoll(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the paced goroutine never waited for a tick")
	}
}

// tick lets the poll run once.
func (s *followStub) tick(t *testing.T) {
	t.Helper()
	select {
	case s.ticks <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("the paced goroutine never took its tick")
	}
}

// The owner poll re-checks the owner once per tick, never before its first
// one, and re-points only when it moved. Stopping it waits for its
// goroutine, so the caller may read vipOwner again.
func TestStartOwnerPollPollsUntilStopped(t *testing.T) {
	master := &movableMaster{name: "gateway-1"}
	e, cmd, buf := followEngine(t, master.respond)
	follow := stubFollowTick(e)
	plumbed := cmd.count("ip route replace")

	// The owner has already moved when the poll starts: the first poll
	// still comes one interval later.
	master.set("gateway-2")
	stop := e.startOwnerPoll(t.Context(), phaseHold)
	follow.awaitPoll(t)
	if got := cmd.count("ip route replace") - plumbed; got != 0 {
		t.Fatalf("the owner poll re-pointed before its first tick with %d route commands: %v", got, cmd.lines())
	}

	follow.tick(t)
	follow.awaitPoll(t)
	if got := cmd.count("ip route replace") - plumbed; got != 2 {
		t.Fatalf("issued %d route commands on the tick after the owner moved, want the "+
			"forward route and the scope-link route: %v", got, cmd.lines())
	}

	// A tick with the owner where it was: nothing to re-point.
	follow.tick(t)
	follow.awaitPoll(t)
	stop()

	if got := cmd.count("ip route replace") - plumbed; got != 2 {
		t.Fatalf("a tick with an unmoved owner issued %d more route commands: %v", got-2, cmd.lines())
	}
	if e.vipOwner != "gateway-2" {
		t.Fatalf("vipOwner = %q after the poll saw the owner move, want gateway-2", e.vipOwner)
	}
	repoints := repointsIn(t, buf.String())
	if len(repoints) != 2 {
		t.Fatalf("journaled %d %s events, want the start one and the move: %+v",
			len(repoints), evVIPRepoint, repoints)
	}
	if got := repoints[1]; got.Phase != phaseHold || got.Target != "gateway-2" || got.Detail != "" {
		t.Fatalf("the move was journaled as %+v, want phase %s and target gateway-2", got, phaseHold)
	}
	if got := follow.stopped.Load(); got != 1 {
		t.Fatalf("stop returned with %d polls stopped, want the one it started", got)
	}
}

// An owner that moves while a fault is being injected is followed at once,
// not at the convergence after the restore. Both polls, the one beside the
// inject and the one beside the hold, are stopped before the restore
// starts.
func TestExecuteFollowsTheOwnerBesideTheInjectAndTheHold(t *testing.T) {
	master := &movableMaster{name: "gateway-1"}
	e, _, buf := followEngine(t, master.respond)
	follow := stubFollowTick(e)

	stoppedAtRestore := int32(-1)
	kill := noopActions("gateway-kill")[0]
	kill.inject = func(context.Context, *lab, string, int) error {
		follow.awaitPoll(t)
		master.set("gateway-2")
		follow.tick(t)
		follow.awaitPoll(t)
		return nil
	}
	kill.restore = func(context.Context, *lab, string) error {
		stoppedAtRestore = follow.stopped.Load()
		return nil
	}

	e.execute(t.Context(), decision{tick: 1, action: kill, target: "gateway-1", hold: 5 * time.Second})

	if stoppedAtRestore != 2 {
		t.Fatalf("%d owner polls were stopped when the restore began, want both "+
			"(the inject's and the hold's)", stoppedAtRestore)
	}
	// What the journal holds after the start re-point: the inject, the
	// re-point that followed the owner during it, then the restore.
	var order []string
	var converged event
	for _, ev := range eventsIn(t, buf.String())[1:] {
		switch ev.Event {
		case evInject, evRestore:
			order = append(order, ev.Event)
		case evVIPRepoint:
			order = append(order, ev.Event+"/"+ev.Phase+"/"+ev.Target)
		case evConverged:
			converged = ev
		}
	}
	want := "inject vip-repoint/inject/gateway-2 restore"
	if got := strings.Join(order, " "); got != want {
		t.Fatalf("journaled %q after the start re-point, want %q", got, want)
	}
	if converged.CROwner != "gateway-2" {
		t.Fatalf("the converged event names cr_owner %q, want gateway-2: %+v", converged.CROwner, converged)
	}
}

// An owner that moves while a fault is held is followed during the hold,
// not at the convergence after the restore.
func TestExecuteFollowsTheOwnerThroughTheHold(t *testing.T) {
	master := &movableMaster{name: "gateway-1"}
	e, _, buf := followEngine(t, master.respond)
	follow := stubFollowTick(e)

	wait, held := e.wait, false
	e.wait = func(ctx context.Context, d time.Duration) bool {
		if !held {
			held = true
			follow.awaitPoll(t)
			master.set("gateway-2")
			follow.tick(t)
			follow.awaitPoll(t)
		}
		return wait(ctx, d)
	}

	kill := noopActions("gateway-kill")[0]
	e.execute(t.Context(), decision{tick: 1, action: kill, target: "gateway-1", hold: 5 * time.Second})

	var order []string
	for _, ev := range eventsIn(t, buf.String())[1:] {
		switch ev.Event {
		case evInject, evRestore:
			order = append(order, ev.Event)
		case evVIPRepoint:
			order = append(order, ev.Event+"/"+ev.Phase+"/"+ev.Target)
		}
	}
	want := "inject vip-repoint/hold/gateway-2 restore"
	if got := strings.Join(order, " "); got != want {
		t.Fatalf("journaled %q after the start re-point, want %q", got, want)
	}
}

// A failed inject is undone by the action's restore, and that restore must
// not run beside the poll either. No hold follows, so no second poll is
// started.
func TestAFailedInjectStopsTheOwnerPollBeforeTheUndo(t *testing.T) {
	e, _, _ := followEngine(t, healthyLabResponses)
	follow := stubFollowTick(e)

	stoppedAtRestore := int32(-1)
	kill := noopActions("gateway-kill")[0]
	kill.inject = func(context.Context, *lab, string, int) error { return errBoom }
	kill.restore = func(context.Context, *lab, string) error {
		stoppedAtRestore = follow.stopped.Load()
		return nil
	}

	e.execute(t.Context(), decision{tick: 1, action: kill, target: "gateway-1", hold: 5 * time.Second})

	if stoppedAtRestore != 1 {
		t.Fatalf("%d owner polls were stopped when the undo's restore began, want the inject's",
			stoppedAtRestore)
	}
	if got := follow.stopped.Load(); got != 1 {
		t.Fatalf("%d owner polls ran for a fault that was never held, want only the inject's", got)
	}
}

// parkedIn reports whether the journal shows gw parked as unconverged —
// the state that keeps a node out of every later draw.
func parkedIn(t *testing.T, journal, gw string) bool {
	t.Helper()
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evNodeState && ev.Target == gw && ev.State == nodeUnconverged {
			return true
		}
	}
	return false
}

// An action the runner cannot inject leaves the lab in a state it cannot
// reason about: the node is parked, never targeted again, and the run is
// stamped a harness fault.
func TestFailedInjectParksTheNodeAndFaultsTheHarness(t *testing.T) {
	actions := noopActions("controller-restart")
	actions[0].inject = func(context.Context, *lab, string, int) error {
		return errBoom
	}

	_, rec := runEngine(t, 42, 5*time.Minute, actions, nil)

	if rec.Result != resultHarnessFault {
		t.Fatalf("result = %q, want %q", rec.Result, resultHarnessFault)
	}
	if len(rec.Violations) == 0 || rec.Violations[0].Kind != violationActionFailed {
		t.Fatalf("violations = %+v, want a %s", rec.Violations, violationActionFailed)
	}
	if !strings.Contains(rec.Violations[0].Detail, "inject") {
		t.Fatalf("violation detail %q does not name the phase that failed", rec.Violations[0].Detail)
	}
}

// An inject is not atomic: the destructive ones pin the docker restart
// policy to "no" before the step that can fail. Bailing out on that error
// without undoing it leaves a gateway docker will never revive — the very
// wreckage the restore path exists to prevent — so the restore runs even
// when it is the inject that failed.
func TestAFailedInjectIsUndone(t *testing.T) {
	actions := noopActions("agent-terminate")
	actions[0].inject = func(context.Context, *lab, string, int) error { return errBoom }
	var restores int
	var restoreCtxErr error
	var bounded bool
	actions[0].restore = func(rctx context.Context, _ *lab, _ string) error {
		restores++
		restoreCtxErr = rctx.Err()
		_, bounded = rctx.Deadline()
		return nil
	}

	journal, rec := runEngine(t, 42, 5*time.Minute, actions, nil)

	if restores != 1 {
		t.Fatalf("the half-injected fault was undone %d times, want exactly once", restores)
	}
	// The inject can fail *because* the run was cancelled underneath it, so
	// the undo runs on the same detached, bounded context the held-fault
	// restore uses.
	if restoreCtxErr != nil {
		t.Fatalf("the undo ran on a context that was already done (%v)", restoreCtxErr)
	}
	if !bounded {
		t.Fatal("the undo ran on a context with no deadline of its own")
	}
	// The action still failed, and it still failed on the inject.
	if len(rec.Violations) == 0 || rec.Violations[0].Kind != violationActionFailed {
		t.Fatalf("violations = %+v, want a %s", rec.Violations, violationActionFailed)
	}
	if !strings.Contains(rec.Violations[0].Detail, "inject") {
		t.Fatalf("violation detail %q does not name the phase that failed", rec.Violations[0].Detail)
	}
	var undone bool
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evRestore && strings.Contains(ev.Detail, "undo") {
			undone = true
		}
	}
	if !undone {
		t.Fatalf("the undo was not journaled: %q", journal)
	}
}

// The same invariant against the real registry, which is where it bites:
// agent-terminate disables the restart policy and then waits for the
// container to follow the agent down. A draining agent can outlive
// agentExitTimeout, and that wait is the step that fails. The gateway must
// not be left pinned to `restart: no` with its containerlab veth gone —
// nothing would ever bring it back, and every scenario after the run would
// need `make e2e-down && make e2e-up` first.
func TestAFailedAgentTerminateHandsTheRestartPolicyBack(t *testing.T) {
	clock := newFakeClock()
	// The container never follows the agent down, so waitContainerExit runs
	// out its budget and the inject fails.
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "{{.State.Running}}") {
			return "true\n", nil
		}
		return healthyLabResponses(argv)
	}}
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       42,
			DurationMS: (5 * time.Minute).Milliseconds(),
			TickMinMS:  (10 * time.Second).Milliseconds(),
			TickMaxMS:  (10 * time.Second).Milliseconds(),
		},
		ActionsByName: map[string]int{},
	}
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), []*action{actionNamed(t, "agent-terminate")},
		greenProbes{}, newJournal(&bytes.Buffer{}, clock.now), rec)
	e.wait, e.now = clock.wait, clock.now

	e.run(context.Background())

	if !cmd.called("update --restart=no") {
		t.Fatalf("the fault was never injected at all: %v", cmd.lines())
	}
	if !cmd.called("update --restart=always") {
		t.Fatalf("the gateway was left pinned to restart=no after a failed inject: %v", cmd.lines())
	}
	if !cmd.called("containerlab tools veth create") {
		t.Fatalf("the underlay veth was not re-created after a failed inject: %v", cmd.lines())
	}
	if len(rec.Violations) == 0 || rec.Violations[0].Kind != violationActionFailed {
		t.Fatalf("violations = %+v, want a %s", rec.Violations, violationActionFailed)
	}
}

// upstreamEndTornDownLate answers every upstream `ip link show ethN` probe
// as present for its first two calls after each veth create (and before the
// first), and as gone otherwise: every restore's re-creation finds the
// previous incarnation's end still there and has to wait for it.
func upstreamEndTornDownLate() func(argv []string) (string, error) {
	probes := 0
	return func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "containerlab tools veth create"):
			probes = 0
			return "", nil
		case strings.Contains(line, "clab-ovn-e2e-upstream ip link show eth"):
			if probes++; probes <= 2 {
				return "", nil
			}
		}
		return healthyLabResponses(argv)
	}
}

// A restore whose veth re-creation had to wait for the previous
// incarnation's upstream end says so in the journal, right after the plain
// `restore` event the engine emits before it. A clean restore journals
// exactly what it always did, and a failed one leaves its trace in the
// action-failed violation, not in a note. The lab's note hook never
// outlives the restore that set it.
func TestARestoreThatWaitedForTheStaleVethIsJournaled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		respond     func(argv []string) (string, error)
		wantNotes   bool
		wantFailure string
	}{
		{
			name:      "the upstream end is torn down late",
			respond:   upstreamEndTornDownLate(),
			wantNotes: true,
		},
		{
			name:    "the upstream end is already gone",
			respond: healthyLabResponses,
		},
		{
			name: "the rewire fails",
			respond: func(argv []string) (string, error) {
				if strings.Contains(strings.Join(argv, " "), "containerlab tools veth create") {
					return "", errBoom
				}
				return healthyLabResponses(argv)
			},
			wantFailure: "re-create underlay veth",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			// Every command takes time, as a real docker exec does, so a clean
			// restore cannot pass for one that waited.
			cmd := &fakeCommander{respond: func(argv []string) (string, error) {
				clock.sleep(500 * time.Millisecond)
				return tc.respond(argv)
			}}
			rec := &runRecord{
				Inputs: runInputs{
					Seed:       42,
					DurationMS: (5 * time.Minute).Milliseconds(),
					TickMinMS:  (10 * time.Second).Milliseconds(),
					TickMaxMS:  (10 * time.Second).Milliseconds(),
				},
				ActionsByName: map[string]int{},
			}
			var buf bytes.Buffer
			e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), []*action{actionNamed(t, "gateway-restart")},
				greenProbes{}, newJournal(&buf, clock.now), rec)
			e.wait, e.now = clock.wait, clock.now

			e.run(context.Background())

			if e.lab.note != nil {
				t.Fatal("the lab's note hook outlived the restore that set it")
			}
			if tc.wantFailure == "" && len(rec.Violations) != 0 {
				t.Fatalf("violations = %+v, want none", rec.Violations)
			}
			if tc.wantFailure != "" && (len(rec.Violations) == 0 ||
				rec.Violations[0].Kind != violationActionFailed ||
				!strings.Contains(rec.Violations[0].Detail, tc.wantFailure)) {
				t.Fatalf("violations = %+v, want a %s naming %q", rec.Violations, violationActionFailed, tc.wantFailure)
			}

			events := eventsIn(t, buf.String())
			noted, prev := 0, -1
			for i, ev := range events {
				if ev.Event != evRestore {
					continue
				}
				if ev.Detail != "" {
					if !tc.wantNotes {
						t.Fatalf("restore event carries a detail: %+v", ev)
					}
					if want := "upstream:" + mustLink(t, ev.Target).upstreamIface; !strings.Contains(ev.Detail, want) {
						t.Fatalf("restore detail %q does not name %s", ev.Detail, want)
					}
					if prev < 0 || events[prev].Tick != ev.Tick || events[prev].Target != ev.Target ||
						events[prev].Detail != "" {
						t.Fatalf("restore note %+v does not follow the plain restore event of its node", ev)
					}
					noted++
				}
				prev = i
			}
			if tc.wantNotes && noted == 0 {
				t.Fatalf("no restore journaled the wait for the stale upstream end: %q", buf.String())
			}
		})
	}
}

// undo restores through the same path, so a re-creation it had to wait for
// is journaled too. There is no plain `restore` event ahead of an undo: the
// note comes first, and the `restore` event reporting the undo follows it.
func TestAnUndoThatWaitedForTheStaleVethIsJournaled(t *testing.T) {
	clock := newFakeClock()
	// The container never follows the agent down, so the inject fails and
	// the undo re-creates the veth while the upstream end is still there.
	late := upstreamEndTornDownLate()
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "{{.State.Running}}") {
			return "true\n", nil
		}
		return late(argv)
	}}
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       42,
			DurationMS: (5 * time.Minute).Milliseconds(),
			TickMinMS:  (10 * time.Second).Milliseconds(),
			TickMaxMS:  (10 * time.Second).Milliseconds(),
		},
		ActionsByName: map[string]int{},
	}
	var buf bytes.Buffer
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), []*action{actionNamed(t, "agent-terminate")},
		greenProbes{}, newJournal(&buf, clock.now), rec)
	e.wait, e.now = clock.wait, clock.now

	e.run(context.Background())

	if e.lab.note != nil {
		t.Fatal("the lab's note hook outlived the undo that set it")
	}
	var restores []event
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event == evRestore {
			restores = append(restores, ev)
		}
	}
	if len(restores) != 2 {
		t.Fatalf("restore events = %+v, want the note and the undo", restores)
	}
	note, undo := restores[0], restores[1]
	if want := "upstream:" + mustLink(t, note.Target).upstreamIface; !strings.Contains(note.Detail, want) {
		t.Fatalf("first restore event %+v is not the note naming %s", note, want)
	}
	if !strings.HasPrefix(undo.Detail, "undo after a failed inject") ||
		undo.Tick != note.Tick || undo.Target != note.Target {
		t.Fatalf("restore event %+v after the note is not the undo of its node", undo)
	}
}

// The tick loop idles out its interval on the real clock. Waited out with
// a bare time.Sleep, a Ctrl-C would only be observed once the current
// interval (up to -tick-max) had run its course.
func TestACancelledRunDoesNotWaitOutItsTickInterval(t *testing.T) {
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       42,
			DurationMS: time.Minute.Milliseconds(),
			TickMinMS:  (30 * time.Second).Milliseconds(),
			TickMaxMS:  (30 * time.Second).Milliseconds(),
		},
		ActionsByName: map[string]int{},
	}
	// The engine keeps its real clock: what is under test is that the wait
	// itself is interruptible, not that a fake one can be advanced past it.
	e := newEngine(newLab("ovn-e2e", &fakeCommander{respond: healthyLabResponses}),
		defaultTestProfile(t), noopActions("controller-restart"), greenProbes{},
		newJournal(&bytes.Buffer{}, time.Now), rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.run(ctx)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled run kept sleeping out its 30s tick interval")
	}
	if rec.Decisions.Executed != 0 {
		t.Fatalf("a cancelled run executed %d actions", rec.Decisions.Executed)
	}
}

// A node that does not come back inside its budget is a
// reachability-recovery violation, and is never re-targeted.
func TestRecoveryBudgetExpiryIsAViolation(t *testing.T) {
	actions := noopActions("gateway-kill")
	actions[0].recoveryBudget = 30 * time.Second

	clock := newFakeClock()
	var buf bytes.Buffer
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       42,
			DurationMS: (2 * time.Minute).Milliseconds(),
			TickMinMS:  (10 * time.Second).Milliseconds(),
			TickMaxMS:  (10 * time.Second).Milliseconds(),
		},
		ActionsByName: map[string]int{},
	}
	// The lab is reachable, but the restarted container never reports
	// healthy — convergence can never be declared.
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "{{.State.Health.Status}}") {
			return "starting\n", nil
		}
		return healthyLabResponses(argv)
	}}
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), actions, greenProbes{},
		newJournal(&buf, clock.now), rec)
	e.wait, e.now = clock.wait, clock.now

	e.run(context.Background())
	rec.finalize(clock.now())

	if len(rec.Violations) == 0 || rec.Violations[0].Kind != violationRecoveryTimeout {
		t.Fatalf("violations = %+v, want a %s", rec.Violations, violationRecoveryTimeout)
	}
	if rec.Result != resultFail {
		t.Fatalf("result = %q, want %q", rec.Result, resultFail)
	}
	if state := e.nodeState(rec.Violations[0].Target); state != nodeUnconverged {
		t.Fatalf("node %s left in state %q, want %q", rec.Violations[0].Target, state, nodeUnconverged)
	}
}

// A converged action records the summed loss since the inject and since
// the restore, keeps the legacy inject-to-last-recovery span, and puts the
// same loss on the converged journal line.
func TestConvergeRecordsSummedDowntime(t *testing.T) {
	t.Run("a probe source that lost time", func(t *testing.T) {
		journal, rec := runEngine(t, 42, 35*time.Second, noopActions("gateway-kill"),
			func(e *engine) { e.probes = windowProbes{} })

		if rec.Decisions.Executed == 0 {
			t.Fatal("the run executed no action, so nothing converged")
		}
		if len(rec.Recoveries) != rec.Decisions.Executed {
			t.Fatalf("%d recovery records for %d executed actions", len(rec.Recoveries), rec.Decisions.Executed)
		}
		// windowProbes answers with its anchor, so each measure must equal
		// the time of the journal line it is anchored at; the noop restore
		// returns at once, so the restore anchor is the restore line's time.
		events := eventsIn(t, journal)
		injected, restored := map[int]int64{}, map[int]int64{}
		for _, ev := range events {
			ts, err := time.Parse(time.RFC3339Nano, ev.TS)
			if err != nil {
				t.Fatalf("journal ts %q: %v", ev.TS, err)
			}
			switch ev.Event {
			case evInject:
				injected[ev.Tick] = ts.UnixMilli()
			case evRestore:
				restored[ev.Tick] = ts.UnixMilli()
			}
		}
		for _, r := range rec.Recoveries {
			if r.DownMS["fip-vm1"] != injected[r.Tick] || r.DownWindows["fip-vm1"] != 2 {
				t.Fatalf("tick %d down_ms/down_windows = %v/%v, want the loss since the inject (%d) in 2 windows",
					r.Tick, r.DownMS, r.DownWindows, injected[r.Tick])
			}
			if r.FromRestoreMS["fip-vm1"] != restored[r.Tick] {
				t.Fatalf("tick %d from_restore_ms = %v, want the loss since the restore (%d)",
					r.Tick, r.FromRestoreMS, restored[r.Tick])
			}
			if r.FromInjectMS["fip-vm1"] != 40_090 {
				t.Fatalf("tick %d from_inject_ms = %v, want recoverySince's span (40090)", r.Tick, r.FromInjectMS)
			}
		}
		first := rec.Recoveries[0]
		raw, err := json.Marshal(first)
		if err != nil {
			t.Fatalf("marshal a recovery: %v", err)
		}
		for _, key := range []string{
			fmt.Sprintf(`"down_ms":{"fip-vm1":%d}`, injected[first.Tick]), `"down_windows":{"fip-vm1":2}`,
			`"from_inject_ms":{"fip-vm1":40090}`, fmt.Sprintf(`"from_restore_ms":{"fip-vm1":%d}`, restored[first.Tick]),
		} {
			if !strings.Contains(string(raw), key) {
				t.Fatalf("the summary.json recovery lacks %s: %s", key, raw)
			}
		}
		converged := 0
		for _, ev := range events {
			if ev.Event != evConverged {
				continue
			}
			converged++
			if ev.DownMS["fip-vm1"] != injected[ev.Tick] || ev.DownWindows["fip-vm1"] != 2 {
				t.Fatalf("converged event down_ms/down_windows = %v/%v, want the loss since the inject (%d) in 2 windows",
					ev.DownMS, ev.DownWindows, injected[ev.Tick])
			}
			if ev.RecoveryMS["fip-vm1"] != restored[ev.Tick] {
				t.Fatalf("converged event recovery_ms = %v, want the loss since the restore (%d), as in from_restore_ms",
					ev.RecoveryMS, restored[ev.Tick])
			}
		}
		if converged != rec.Decisions.Executed {
			t.Fatalf("%d converged events for %d executed actions", converged, rec.Decisions.Executed)
		}
	})

	t.Run("a probe source that lost nothing", func(t *testing.T) {
		journal, rec := runEngine(t, 42, 35*time.Second, noopActions("gateway-kill"), nil)

		if rec.Decisions.Executed == 0 {
			t.Fatal("the run executed no action, so nothing converged")
		}
		// omitempty drops a nil map, not one holding a zero entry: a
		// reader sees every probe on every converged line.
		converged := 0
		for _, ev := range eventsIn(t, journal) {
			if ev.Event != evConverged {
				continue
			}
			converged++
			ms, msOK := ev.DownMS["fip-vm1"]
			windows, windowsOK := ev.DownWindows["fip-vm1"]
			if !msOK || !windowsOK || ms != 0 || windows != 0 {
				t.Fatalf("converged event down_ms/down_windows = %v/%v, want a 0 entry for fip-vm1",
					ev.DownMS, ev.DownWindows)
			}
		}
		if converged == 0 {
			t.Fatal("the journal carries no converged event")
		}
	})
}

func TestParseWeights(t *testing.T) {
	actions := noopActions("controller-restart", "gateway-kill")

	tests := []struct {
		name    string
		spec    string
		want    map[string]int
		wantErr string
	}{
		{
			name: "empty spec keeps the registry defaults",
			spec: "",
			want: map[string]int{"controller-restart": 1, "gateway-kill": 1},
		},
		{
			name: "named actions are overridden",
			spec: "gateway-kill=0, controller-restart=5",
			want: map[string]int{"controller-restart": 5, "gateway-kill": 0},
		},
		{
			name:    "unknown action is rejected",
			spec:    "gateway-melt=3",
			wantErr: `unknown action "gateway-melt"`,
		},
		{
			name:    "malformed pair is rejected",
			spec:    "gateway-kill",
			wantErr: "is not name=value",
		},
		{
			name:    "non-numeric weight is rejected",
			spec:    "gateway-kill=lots",
			wantErr: `weight for "gateway-kill"`,
		},
		{
			name:    "negative weight is rejected",
			spec:    "gateway-kill=-1",
			wantErr: "is negative",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseWeights(tc.spec, actions)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseWeights(%q) = %v, want an error", tc.spec, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWeights(%q): %v", tc.spec, err)
			}
			for name, want := range tc.want {
				if got[name] != want {
					t.Fatalf("weight[%s] = %d, want %d", name, got[name], want)
				}
			}
		})
	}
}

func slicesEqual(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The flip index is the fifth value every tick draws, and it is drawn
// whether or not the action that was picked reads it. Two runs with the
// same seed must therefore agree on the flips as well as on the actions —
// the profile and the seed together are what a replay reproduces.
func TestSameSeedReplaysTheSameFlips(t *testing.T) {
	actions := func() []*action {
		acts := noopActions("gateway-kill")
		acts[0].usesFlip = true
		return acts
	}
	first, _ := runEngine(t, 7, 20*time.Minute, actions(), nil)
	second, _ := runEngine(t, 7, 20*time.Minute, actions(), nil)

	a, b := decisionsIn(t, first), decisionsIn(t, second)
	if len(a) == 0 {
		t.Fatal("the run made no decisions at all")
	}
	if len(a) != len(b) || !slicesEqual(a, b) {
		t.Fatalf("the same seed drew different flips:\n%v\n%v", a, b)
	}

	drawn := map[string]bool{}
	for _, ev := range eventsIn(t, first) {
		if ev.Event == evDecision && ev.Flip != "" {
			drawn[ev.Flip] = true
		}
	}
	if len(drawn) < 2 {
		t.Fatalf("a 20-minute run drew only %v — the flip is not being drawn per tick", drawn)
	}
}

// A flip that means nothing on the target's current configuration — a
// masquerade variant on a gateway with no VIP — is a guardrail skip. Like
// every other skip it must not shift the stream: the run that skipped
// draws the same values afterwards as the run that executed.
func TestAnInapplicableFlipIsSkippedWithoutShiftingTheStream(t *testing.T) {
	flippable := noopActions("config-flip")
	flippable[0].usesFlip = true
	flippable[0].applicable = func(context.Context, string, int) bool { return true }

	applied := noopActions("config-flip")
	applied[0].usesFlip = true
	// gateway-2's configuration has nothing the drawn flip can change.
	applied[0].applicable = func(_ context.Context, gw string, _ int) bool { return gw != "gateway-2" }

	open, _ := runEngine(t, 42, 20*time.Minute, flippable, nil)
	guarded, rec := runEngine(t, 42, 20*time.Minute, applied, nil)

	a, b := decisionsIn(t, open), decisionsIn(t, guarded)
	for i := range a {
		if i < len(b) && a[i] != b[i] {
			t.Fatalf("decision %d diverged after an inapplicable flip was skipped: %q vs %q", i+1, a[i], b[i])
		}
	}
	if rec.Decisions.Skipped == 0 {
		t.Fatal("no decision was skipped even though every gateway-2 flip was inapplicable")
	}
	var skipped event
	for _, ev := range eventsIn(t, guarded) {
		if ev.Event == evDecision && ev.SkipReason == skipFlipNotApplicable {
			skipped = ev
		}
	}
	if skipped.Target != "gateway-2" {
		t.Fatalf("journaled %+v, want a %s skip on gateway-2", skipped, skipFlipNotApplicable)
	}
	// A skipped decision still says which flip it would have been —
	// otherwise the journal cannot explain why it was skipped.
	if skipped.Flip == "" {
		t.Fatalf("the skipped decision does not name the flip it drew: %+v", skipped)
	}
}

// A profile without the port-forward layer has no Load_Balancer VIP: there
// are no hand-plumbed routes to follow the master with, and issuing them
// would plumb a VIP the run never put up.
func TestFollowMasterDoesNothingWithoutTheLoadBalancerVIP(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	clock := newFakeClock()
	e := newEngine(newTestLab(cmd, clock), testProfile(t, "pf-only"), nil, greenProbes{},
		newJournal(&bytes.Buffer{}, clock.now), &runRecord{ActionsByName: map[string]int{}})
	e.now = clock.now

	e.followMaster(context.Background(), phaseConverge)

	if len(cmd.lines()) != 0 {
		t.Fatalf("a profile without the port-forward layer still plumbed the VIP: %v", cmd.lines())
	}
	if e.vipOwner != "" {
		t.Fatalf("vipOwner = %q on a run with no Load_Balancer VIP", e.vipOwner)
	}

	// With no routes to follow, no owner poll starts either.
	var ticks atomic.Int32
	e.followTick = func(context.Context) bool {
		ticks.Add(1)
		return false
	}
	stop := e.startOwnerPoll(context.Background(), phaseHold)
	stop()

	if got := ticks.Load(); got != 0 {
		t.Fatalf("a profile without the port-forward layer started the owner poll: %d ticks", got)
	}
	if len(cmd.lines()) != 0 {
		t.Fatalf("a profile without the port-forward layer polled for the owner: %v", cmd.lines())
	}
}

// A central-scoped action targets the shared central node and discards the
// gateway the tick drew. The draw still happens, so the stream stays
// aligned: a run of a central action draws the same intervals, holds and
// flips as a run of the same action scoped to a gateway.
func TestCentralActionKeepsTheDrawStreamAligned(t *testing.T) {
	central := noopActions("nb-pause")
	central[0].scope = scopeCentral
	gateway := noopActions("nb-pause") // scopeGateway (zero value)

	centralJournal, _ := runEngine(t, 42, 20*time.Minute, central, nil)
	gatewayJournal, _ := runEngine(t, 42, 20*time.Minute, gateway, nil)

	c := decisionEventsIn(t, centralJournal)
	g := decisionEventsIn(t, gatewayJournal)
	if len(c) == 0 || len(c) != len(g) {
		t.Fatalf("central run drew %d decisions, gateway run %d", len(c), len(g))
	}
	for i := range c {
		if c[i].IntervalMS != g[i].IntervalMS || c[i].HoldMS != g[i].HoldMS || c[i].Flip != g[i].Flip {
			t.Fatalf("decision %d diverged: the discarded gateway draw shifted the stream", i+1)
		}
		if c[i].Target != centralNode {
			t.Fatalf("central action decision %d targeted %q, want %q", i+1, c[i].Target, centralNode)
		}
	}
}

// A gateway-pair fault disrupts and restores both its target and the
// ring-next peer, and its recovery record and converged event name the
// peer so a double failover can be triaged from the artifacts.
func TestPairActionDisruptsRestoresAndConvergesBothNodes(t *testing.T) {
	restored := map[string]int{}
	acts := noopActions("double-failover")
	acts[0].scope = scopeGatewayPair
	acts[0].holdMin, acts[0].holdMax = 0, 0
	acts[0].restore = func(_ context.Context, _ *lab, node string) error {
		restored[node]++
		return nil
	}

	journal, rec := runEngine(t, 42, 5*time.Minute, acts, nil)

	if rec.Decisions.Executed == 0 {
		t.Fatal("no pair fault executed")
	}
	total := 0
	for _, n := range restored {
		total += n
	}
	if total != 2*rec.Decisions.Executed {
		t.Fatalf("restored %d nodes for %d executed pair faults, want two per fault", total, rec.Decisions.Executed)
	}
	if len(rec.Recoveries) != rec.Decisions.Executed {
		t.Fatalf("recorded %d recoveries for %d executions", len(rec.Recoveries), rec.Decisions.Executed)
	}
	for _, r := range rec.Recoveries {
		if r.Peer == "" || r.Peer != nextGateway(r.Target) {
			t.Fatalf("recovery %+v does not name the ring-next peer", r)
		}
	}
	disrupted := map[string]bool{}
	var peeredConverged int
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evNodeState && ev.State == nodeDisrupted {
			disrupted[ev.Target] = true
		}
		if ev.Event == evConverged && ev.Peer != "" && ev.Peer == nextGateway(ev.Target) {
			peeredConverged++
		}
	}
	if peeredConverged != rec.Decisions.Executed {
		t.Fatalf("journaled %d converged events naming a peer, want %d", peeredConverged, rec.Decisions.Executed)
	}
	for _, r := range rec.Recoveries {
		if !disrupted[r.Target] || !disrupted[r.Peer] {
			t.Fatalf("fault on %s/%s did not mark both nodes disrupted", r.Target, r.Peer)
		}
	}
}

// Convergence is gated by a per-scope signal: a central node's container
// health (both databases answering), an upstream node's bgpd. A node that
// never returns by that signal times out and is never wrongly re-targeted.
func TestConvergenceDispatchesPerNodeKind(t *testing.T) {
	tests := []struct {
		name    string
		scope   int
		respond func([]string) (string, error)
	}{
		{
			name:  "central action never converges while central is unhealthy",
			scope: scopeCentral,
			respond: func(argv []string) (string, error) {
				if strings.Contains(strings.Join(argv, " "), "{{.State.Health.Status}}") {
					return "unhealthy\n", nil
				}
				return healthyLabResponses(argv)
			},
		},
		{
			name:  "upstream action never converges while bgpd is down",
			scope: scopeUpstream,
			respond: func(argv []string) (string, error) {
				if strings.Contains(strings.Join(argv, " "), "pgrep -f "+bgpdMatchPattern) {
					return "", errExit(t, 1)
				}
				return healthyLabResponses(argv)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acts := noopActions("control-plane")
			acts[0].scope = tc.scope
			acts[0].recoveryBudget = 30 * time.Second
			acts[0].holdMin, acts[0].holdMax = 0, 0

			_, rec := runEngine(t, 42, 2*time.Minute, acts, func(e *engine) {
				e.lab.cmd = &fakeCommander{respond: tc.respond}
			})

			if len(rec.Violations) == 0 || rec.Violations[0].Kind != violationRecoveryTimeout {
				t.Fatalf("violations = %+v, want a %s", rec.Violations, violationRecoveryTimeout)
			}
		})
	}
}

// The flip is named on a decision only for the one action that reads it —
// config-flip, which sets usesFlip. A drift-style action reads live state
// through applicable but never touches the flip, so its decisions must not
// carry one.
func TestOnlyFlipAwareActionsJournalTheFlip(t *testing.T) {
	acts := noopActions("kernel-route-drop")
	acts[0].applicable = func(context.Context, string, int) bool { return true }

	journal, _ := runEngine(t, 42, 20*time.Minute, acts, nil)

	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evDecision && ev.Flip != "" {
			t.Fatalf("a non-flip action named a flip on its decision: %+v", ev)
		}
	}
}

// decisionEventsIn extracts the decision events from a journal, in order.
func decisionEventsIn(t *testing.T, journal string) []event {
	t.Helper()
	var out []event
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evDecision {
			out = append(out, ev)
		}
	}
	return out
}

// runEngineOracle drives a full engine run wired to a config-aware oracle over
// the oracleLab fixture, with settle windows at the given cadence. The engine
// and the oracle share one fake clock, so a settle consumes the run's
// wall-clock exactly as it would in production while the whole run still
// completes in microseconds. It returns the journal and the run record.
func runEngineOracle(t *testing.T, seed int64, duration, settleEvery time.Duration,
	actions []*action, fx *oracleLab) (string, *runRecord) {
	t.Helper()

	clock := newFakeClock()
	lab := newTestLab(&fakeCommander{respond: fx.respond}, clock)
	var buf bytes.Buffer
	jrnl := newJournal(&buf, clock.now)

	rec := &runRecord{
		Inputs: runInputs{
			Seed:            seed,
			DurationMS:      duration.Milliseconds(),
			TickMinMS:       (10 * time.Second).Milliseconds(),
			TickMaxMS:       (30 * time.Second).Milliseconds(),
			SettleEveryMS:   settleEvery.Milliseconds(),
			SettleTimeoutMS: (90 * time.Second).Milliseconds(),
			Lab:             "ovn-e2e",
		},
		ActionsByName: map[string]int{},
	}
	orc := newOracle(lab, oracleApplier(fullModeDocs(t)))
	orc.settleTimeout = time.Duration(rec.Inputs.SettleTimeoutMS) * time.Millisecond
	if err := orc.prime(context.Background()); err != nil {
		t.Fatalf("prime the oracle: %v", err)
	}

	e := newEngine(lab, defaultTestProfile(t), actions, greenProbes{}, jrnl, rec)
	e.wait, e.now = clock.wait, clock.now
	e.oracle = orc
	e.settleEvery = time.Duration(rec.Inputs.SettleEveryMS) * time.Millisecond

	e.run(context.Background())
	rec.finalize(clock.now())
	return buf.String(), rec
}

// The settle windows run on the configured cadence between ticks — never
// between an inject and its restore, where the lab is deliberately broken.
func TestSettleWindowsRunAtTheConfiguredCadenceBetweenTicks(t *testing.T) {
	fx := newOracleLab(t)
	journal, rec := runEngineOracle(t, 42, 10*time.Minute, 30*time.Second,
		noopActions("controller-restart"), fx)

	evs := eventsIn(t, journal)
	var starts, results int
	injecting := false
	settleAfterDecision := false
	sawDecision := false
	for _, ev := range evs {
		switch ev.Event {
		case evDecision:
			sawDecision = true
		case evInject:
			injecting = true
		case evRestore:
			injecting = false
		case evSettleStart:
			starts++
			if injecting {
				t.Fatal("a settle window opened between an inject and its restore")
			}
			if sawDecision {
				settleAfterDecision = true
			}
		case evSettleResult:
			results++
			if injecting {
				t.Fatal("a settle result landed between an inject and its restore")
			}
		}
	}

	if starts == 0 {
		t.Fatal("no settle window ran at the configured cadence")
	}
	if starts != results {
		t.Fatalf("%d settle-start events but %d settle-result events", starts, results)
	}
	if !settleAfterDecision {
		t.Fatal("every settle ran before the first decision, not between ticks")
	}
	if len(rec.Settles) != starts {
		t.Fatalf("recorded %d settles for %d settle windows", len(rec.Settles), starts)
	}
	// Every settle over the green fixture converges cleanly.
	for _, s := range rec.Settles {
		if !s.Passed || s.ConvergedMS < 0 {
			t.Fatalf("a settle over a converged lab did not pass: %+v", s)
		}
	}
	if len(rec.Violations) != 0 {
		t.Fatalf("a green run recorded violations: %+v", rec.Violations)
	}
}

// Settles consume the run's wall-clock but draw nothing from the rng, so the
// decision stream a replay reproduces is identical whether or not they run.
func TestSettleDoesNotShiftTheDecisionStream(t *testing.T) {
	withSettles, settledRec := runEngineOracle(t, 42, 15*time.Minute, 30*time.Second,
		noopActions("controller-restart", "gateway-kill"), newOracleLab(t))
	without, _ := runEngineOracle(t, 42, 15*time.Minute, 0,
		noopActions("controller-restart", "gateway-kill"), newOracleLab(t))

	if len(settledRec.Settles) == 0 {
		t.Fatal("the settled run ran no settle windows, so it proves nothing")
	}

	a, b := decisionsIn(t, withSettles), decisionsIn(t, without)
	if len(a) == 0 {
		t.Fatal("the settled run made no decisions at all")
	}
	// Settles only consume time; they never add ticks, so the settled run
	// fits no more decisions than the unsettled one.
	if len(a) > len(b) {
		t.Fatalf("the settled run fit more decisions (%d) than the unsettled one (%d)", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("decision %d diverged when settles were enabled: %q vs %q", i+1, a[i], b[i])
		}
	}
}

// A settle violation is stamped with the current tick and the journal offset
// of the last executed action, so a reader jumps straight from the violation
// in the record to the inject that preceded it in the journal.
func TestSettleViolationsCarryTheLastActionsJournalOffset(t *testing.T) {
	fx := newOracleLab(t)
	fx.dropKernel["gateway-1"] = "192.0.2.10" // a kernel route missing on every poll

	journal, rec := runEngineOracle(t, 42, 10*time.Minute, 30*time.Second,
		noopActions("controller-restart"), fx)

	var got *violationRecord
	for i := range rec.Violations {
		if r := &rec.Violations[i]; r.Kind == violationExpectedState &&
			r.Target == "gateway-1" && strings.Contains(r.Detail, "kernel") {
			got = r
			break
		}
	}
	if got == nil {
		t.Fatalf("the settle over the red fixture recorded no kernel violation: %+v", rec.Violations)
	}
	if got.Tick == 0 {
		t.Fatalf("the settle violation was not stamped with a tick: %+v", got)
	}

	// jrnl.count() right after an emit is that line's 1-based number, so the
	// stamped offset must be the line of the inject that preceded the first
	// settle window.
	evs := eventsIn(t, journal)
	firstSettle := -1
	for i, ev := range evs {
		if ev.Event == evSettleStart {
			firstSettle = i
			break
		}
	}
	if firstSettle < 0 {
		t.Fatal("no settle window ran")
	}
	injectLine := -1
	for i := 0; i < firstSettle; i++ {
		if evs[i].Event == evInject {
			injectLine = i + 1
		}
	}
	if injectLine < 0 {
		t.Fatal("no inject preceded the first settle window")
	}
	if got.JournalOffset != injectLine {
		t.Fatalf("violation journal offset = %d, want the preceding inject's line %d",
			got.JournalOffset, injectLine)
	}
}

// injectDrains runs a short engine over agent-terminate faults, answering
// through respond, and returns the drain every inject event carried: "on",
// "off", or "" for an event without one. withOracle wires the config-aware
// oracle over fx the way the runner does.
func injectDrains(t *testing.T, fx *oracleLab, respond func([]string) (string, error), withOracle bool) []string {
	t.Helper()
	clock := newFakeClock()
	lab := newTestLab(&fakeCommander{respond: respond}, clock)
	var buf bytes.Buffer
	rec := &runRecord{
		Inputs: runInputs{
			Seed: 42, DurationMS: (5 * time.Minute).Milliseconds(),
			TickMinMS: (10 * time.Second).Milliseconds(), TickMaxMS: (30 * time.Second).Milliseconds(),
			Lab: "ovn-e2e",
		},
		ActionsByName: map[string]int{},
	}
	e := newEngine(lab, defaultTestProfile(t), noopActions("agent-terminate"), greenProbes{},
		newJournal(&buf, clock.now), rec)
	e.wait, e.now = clock.wait, clock.now
	if withOracle {
		orc := newOracle(lab, oracleApplier(fullModeDocs(t)))
		if err := orc.prime(context.Background()); err != nil {
			t.Fatalf("prime the oracle: %v", err)
		}
		e.oracle = orc
	}
	e.run(context.Background())

	var drains []string
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event != evInject {
			continue
		}
		switch {
		case ev.Drain == nil:
			drains = append(drains, "")
		case *ev.Drain:
			drains = append(drains, "on")
		default:
			drains = append(drains, "off")
		}
	}
	if len(drains) == 0 {
		t.Fatal("the run injected nothing")
	}
	return drains
}

// The inject event carries the drain the target ran with, so a report can
// tell a drained restart from one that was not — and carries none when the
// question could not be asked or nobody was there to ask it.
func TestInjectEventCarriesTheEffectiveDrain(t *testing.T) {
	drainOn := newOracleLab(t)
	drainOn.drainEnv = "true" // no marker: the deploy-time env decides
	failing := newOracleLab(t)
	tests := []struct {
		name       string
		fx         *oracleLab
		respond    func([]string) (string, error)
		withOracle bool
		want       string
	}{
		{"drain on", drainOn, drainOn.respond, true, "on"},
		{"drain off", newOracleLab(t), nil, true, "off"},
		{"the drain question fails", failing, func(argv []string) (string, error) {
			if strings.Contains(strings.Join(argv, " "), "test -f "+profileMarkerPath) {
				return "", errBoom
			}
			return failing.respond(argv)
		}, true, ""},
		{"no oracle", nil, healthyLabResponses, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			respond := tc.respond
			if respond == nil {
				respond = tc.fx.respond
			}
			for i, got := range injectDrains(t, tc.fx, respond, tc.withOracle) {
				if got != tc.want {
					t.Fatalf("inject %d carried drain %q, want %q", i, got, tc.want)
				}
			}
		})
	}
}

// driftScrape is one scripted answer of the agent's metrics endpoint.
type driftScrape struct {
	kernel, frr int
	err         error
}

// driftScrapeLab answers every scrape of the metrics endpoint from the script,
// in order, and every other query like a healthy lab. A scrape past the end of
// the script repeats the last entry.
func driftScrapeLab(script ...driftScrape) *fakeCommander {
	var scrapes int
	return &fakeCommander{respond: func(argv []string) (string, error) {
		if !strings.Contains(strings.Join(argv, " "), "/dev/tcp/127.0.0.1/9273") {
			return healthyLabResponses(argv)
		}
		s := script[min(scrapes, len(script)-1)]
		scrapes++
		if s.err != nil {
			return "", s.err
		}
		return fmt.Sprintf("# TYPE ovn_network_agent_route_drift_total counter\n"+
			"ovn_network_agent_route_drift_total{kind=\"frr\"} %d\n"+
			"ovn_network_agent_route_drift_total{kind=\"kernel\"} %d\n", s.frr, s.kernel), nil
	}}
}

// A route-drop action brackets its fault with two reads of the agent's drift
// counters and records the difference on the recovery. The difference is
// shown in the report and never asserted on: a read that fails, or a counter
// that went down because the agent restarted, leaves the field out and adds no
// violation.
func TestRouteDropRecordsTheDriftCounterDelta(t *testing.T) {
	const scrape = "/dev/tcp/127.0.0.1/9273"
	tests := []struct {
		name        string
		counts      bool
		script      []driftScrape
		want        *routeDrift
		wantScrapes int
		wantError   bool
	}{
		{
			name:        "both reads succeed",
			counts:      true,
			script:      []driftScrape{{kernel: 1}, {kernel: 2}},
			want:        &routeDrift{Kernel: 1, FRR: 0},
			wantScrapes: 2,
		},
		{
			name:        "the read before the inject fails",
			counts:      true,
			script:      []driftScrape{{err: errBoom}, {kernel: 2}},
			wantScrapes: 1,
			wantError:   true,
		},
		{
			name:        "the read after the convergence fails",
			counts:      true,
			script:      []driftScrape{{kernel: 1}, {err: errBoom}},
			wantScrapes: 2,
			wantError:   true,
		},
		{
			name:        "a counter went down between the reads",
			counts:      true,
			script:      []driftScrape{{kernel: 5, frr: 2}, {kernel: 0, frr: 3}},
			wantScrapes: 2,
		},
		{
			name:   "an action that does not count drift",
			script: []driftScrape{{kernel: 1}, {kernel: 2}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actions := noopActions("kernel-route-drop")
			actions[0].countsRouteDrift = tc.counts
			cmd := driftScrapeLab(tc.script...)

			journal, rec := runEngine(t, 42, 35*time.Second, actions, func(e *engine) { e.lab.cmd = cmd })

			if rec.Decisions.Executed != 1 || len(rec.Recoveries) != 1 {
				t.Fatalf("executed %d actions with %d recoveries, want one of each",
					rec.Decisions.Executed, len(rec.Recoveries))
			}
			recovery := rec.Recoveries[0]
			if got := recovery.RouteDrift; (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("route_drift = %+v, want %+v", got, tc.want)
			}
			raw, err := json.Marshal(recovery)
			if err != nil {
				t.Fatalf("marshal the recovery: %v", err)
			}
			if tc.want != nil {
				if want := `"route_drift":{"kernel":1,"frr":0}`; !strings.Contains(string(raw), want) {
					t.Errorf("the summary.json recovery lacks %s: %s", want, raw)
				}
			} else if strings.Contains(string(raw), "route_drift") {
				t.Errorf("the summary.json recovery carries route_drift: %s", raw)
			}
			if got := cmd.count(scrape); got != tc.wantScrapes {
				t.Errorf("scraped the metrics endpoint %d times, want %d", got, tc.wantScrapes)
			}

			var checkErrors []event
			for _, ev := range eventsIn(t, journal) {
				if ev.Event == evCheckError {
					checkErrors = append(checkErrors, ev)
				}
			}
			if !tc.wantError {
				if len(checkErrors) != 0 {
					t.Errorf("journaled check errors %+v, want none", checkErrors)
				}
			} else {
				want := "read the route drift counters on " + recovery.Target
				if len(checkErrors) != 1 || !strings.Contains(checkErrors[0].Detail, want) ||
					checkErrors[0].Action != "kernel-route-drop" || checkErrors[0].Target != recovery.Target {
					t.Errorf("journaled check errors %+v, want one for the action naming %q", checkErrors, want)
				}
			}
			if len(rec.Violations) != 0 {
				t.Errorf("violations = %+v, want none", rec.Violations)
			}
		})
	}
}
