package main

import (
	"context"
	"strings"
	"time"
)

// =============================================================================
// Fault trace
// =============================================================================
//
// A fault trace follows two things once a second while a traced action runs:
// the chassis that owns each chassisredirect port, and the gateways the
// upstream router forwards each announced prefix over. It journals what
// changes, so a green run says which chassis took a port and when the upstream
// moved, without any lab log. The trace is diagnostic: nothing asserts on it.

// faultTraceInterval is how often the fault trace takes a reading.
const faultTraceInterval = time.Second

// The values a trace event carries where no chassis or gateway can be named,
// and the detail of the events of a reader's first reading.
const (
	traceUnbound  = "unbound" // a chassisredirect port no chassis owns
	traceAbsent   = "absent"  // a port or prefix an earlier reading saw and this one does not
	traceNone     = "none"    // a prefix with no selected gateway path
	traceBaseline = "baseline"
)

// traceReader is one of the two things a fault trace follows. read returns the
// current value of every object. values is what the last successful reading
// returned, with the objects that have since gone kept as traceAbsent. primed
// says a reading has succeeded, which an empty one does too. lastErr is the
// text of the read error journaled last, "" after a success.
type traceReader struct {
	event   string
	read    func(ctx context.Context, l *lab) (map[string]string, error)
	values  map[string]string
	primed  bool
	lastErr string
}

// startFaultTrace follows the chassisredirect owners and the upstream's
// selected paths for one traced action: it takes one reading at once, the
// baseline, then one per traceTick until stop is called. stop cancels the trace
// and waits for its goroutine to return. For an action that is not traced it
// reads nothing.
//
// The trace runs beside the restores, which set lab.note without a lock, so it
// calls only crPortOwners and upstreamSelected: neither reads that field. It
// draws nothing from e.rng, so it does not shift the decision stream.
func (e *engine) startFaultTrace(ctx context.Context, d decision) (stop func()) {
	if !d.action.faultTrace {
		return func() {}
	}
	readers := []*traceReader{
		{event: evCROwner, read: traceCROwners},
		{event: evUpstreamPath, read: traceUpstreamPaths},
	}
	take := func(ctx context.Context) {
		for _, r := range readers {
			e.traceReading(ctx, d, r)
		}
	}

	take(ctx)
	traceCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for e.traceTick(traceCtx) {
			take(traceCtx)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// traceReading takes one reading of r and journals every value that differs
// from the previous one, in object order. The first successful reading is the
// baseline: one event per object, with no from. A failed read keeps the
// previous values and is journaled as a check-error, once per error text until
// a read succeeds again.
func (e *engine) traceReading(ctx context.Context, d decision, r *traceReader) {
	now, err := r.read(ctx, e.lab)
	if err != nil {
		// A stopped trace kills its in-flight command; that is not a lab
		// failure.
		if ctx.Err() != nil {
			return
		}
		if text := err.Error(); text != r.lastErr {
			r.lastErr = text
			e.jrnl.emit(event{Event: evCheckError, Tick: d.tick, Action: d.action.name,
				Detail: "fault trace: " + text})
		}
		return
	}
	r.lastErr = ""

	if !r.primed {
		r.primed, r.values = true, now
		for _, object := range sortedKeys(now) {
			e.jrnl.emit(event{Event: r.event, Tick: d.tick, Action: d.action.name,
				Object: object, To: now[object], Detail: traceBaseline})
		}
		return
	}

	for object := range r.values {
		if _, ok := now[object]; !ok {
			now[object] = traceAbsent
		}
	}
	for _, object := range sortedKeys(now) {
		from, seen := r.values[object]
		if !seen {
			from = traceAbsent
		}
		if from != now[object] {
			e.jrnl.emit(event{Event: r.event, Tick: d.tick, Action: d.action.name,
				Object: object, From: from, To: now[object]})
		}
	}
	r.values = now
}

// traceCROwners reads the owner of every chassisredirect port: its chassis
// name, or traceUnbound.
func traceCROwners(ctx context.Context, l *lab) (map[string]string, error) {
	owners, _, err := crPortOwners(ctx, l)
	if err != nil {
		return nil, err
	}
	for port, chassis := range owners {
		if chassis == "" {
			owners[port] = traceUnbound
		}
	}
	return owners, nil
}

// traceUpstreamPaths reads the gateways the upstream router forwards each
// prefix over: their names joined with a comma, or traceNone.
func traceUpstreamPaths(ctx context.Context, l *lab) (map[string]string, error) {
	selected, err := upstreamSelected(ctx, l)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string, len(selected))
	for prefix, gateways := range selected {
		paths[prefix] = traceNone
		if len(gateways) > 0 {
			paths[prefix] = strings.Join(gateways, ",")
		}
	}
	return paths, nil
}
