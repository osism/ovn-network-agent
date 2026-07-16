package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node lifecycle. A node is only eligible as a fault target while it is
// `healthy` — the issue's "a node is only re-killed after it has
// returned and converged". A node the runner could not bring back is
// parked in `unconverged` and never targeted again; the run fails on the
// violation that put it there and stops injecting (see park).
const (
	nodeHealthy     = "healthy"
	nodeDisrupted   = "disrupted"
	nodeConverging  = "converging"
	nodeUnconverged = "unconverged"
)

// Guardrail skip reasons, journaled on the decision they blocked.
const (
	skipTargetNotHealthy  = "target-not-healthy"
	skipPeerNotHealthy    = "peer-not-healthy"
	skipNoHealthyPeer     = "no-healthy-peer"
	skipNoWeightedAction  = "no-weighted-action"
	skipFlipNotApplicable = "flip-not-applicable"
	skipNotApplicable     = "not-applicable"
)

// Target scopes. An action targets a gateway by default (the zero value);
// the control-plane and routing-flap classes reach the shared central and
// upstream nodes instead, and the double-failover class hits a pair of
// gateways at once. The scope is what the engine reads to decide which
// node states it may target and how many nodes one fault disrupts.
const (
	scopeGateway     = 0
	scopeCentral     = 1
	scopeUpstream    = 2
	scopeGatewayPair = 3
)

// Violation kinds.
const (
	violationRecoveryTimeout = "recovery-timeout"
	violationActionFailed    = "action-failed"
	violationAgentDown       = "agent-down"
	violationDualClaim       = "dual-claim"
	violationProfileApply    = "profile-not-applied"
	violationStartState      = "start-state-not-green"
	violationChecksNeverRan  = "checks-never-ran"
	violationExpectedState   = "expected-state"
	violationRouteFlap       = "route-flap"
	violationDrainDisabled   = "drain-while-disabled"
	violationOVNTouched      = "ovn-touched-in-pf-only"
	violationOracleSetup     = "oracle-setup"
)

// convergePollInterval is how often the engine re-checks a recovering
// node against its recovery budget.
const convergePollInterval = 5 * time.Second

// restoreTimeout backstops one restore. It is deliberately not the
// action's recovery budget: the budget is the SLO the *lab* is held to
// once the node is back, while the restore is the runner reassembling
// what a container lifecycle event destroyed. The longest restore path
// (a gateway-kill on the workload host) chains four wait loops that sum
// to 190s on their own — bounding it by the 180s budget would cut a slow
// but healthy bring-up off half-way and report the runner's own
// arithmetic as an action the lab could not recover from. Every command
// underneath is bounded by cmdTimeout, so this is a backstop against a
// wedged restore, not the thing that paces it.
const restoreTimeout = 5 * time.Minute

// action is one fault the engine can inject. inject and restore are the
// two halves of the fault; between them the engine holds the fault for a
// seeded duration drawn from [holdMin, holdMax].
type action struct {
	name           string
	weight         int
	holdMin        time.Duration
	holdMax        time.Duration
	recoveryBudget time.Duration
	inject         func(ctx context.Context, l *lab, target string, flip int) error
	restore        func(ctx context.Context, l *lab, target string) error

	// scope is one of the scope* constants: which node the fault targets and
	// how many nodes it disrupts. It is the guardrail declaration the issue
	// asks every action to carry — the node states it may target — and the
	// engine reads it when it draws the target, checks the guardrails,
	// restores the fault and gates on convergence.
	scope int

	// object annotates the decision journal with what the fault touched — a
	// route prefix, an nftables table, a database server — so a mixed-class
	// run can be triaged from the artifacts alone. It is static per action.
	object string

	// usesFlip is set only by config-flip: it is the one action whose
	// behaviour depends on the flip index every decision draws, and so the
	// only one whose journal names the drawn flip.
	usesFlip bool

	// applicable reports whether the drawn fault means anything on its
	// target's current state — the config-flip whose toggle changes nothing
	// on the drawn gateway, the drift action whose object the gateway does
	// not carry. An inapplicable fault is a journaled skip, not a rewrite or
	// a no-op deletion. It is nil on every action that always applies.
	applicable func(ctx context.Context, target string, flip int) bool
}

// probeSource is the slice of the prober the engine consumes.
type probeSource interface {
	allGreen() bool
	redTargets() []string
	recoverySince(anchor time.Time) map[string]int64
}

// engine drives one chaos run. Every decision it makes is drawn from
// `rng`, which is seeded from the run's inputs alone — so identical
// inputs replay the identical decision sequence.
type engine struct {
	rng      *rand.Rand
	lab      *lab
	profile  *profile
	actions  []*action
	probes   probeSource
	jrnl     *journal
	rec      *runRecord
	duration time.Duration
	tickMin  time.Duration
	tickMax  time.Duration

	// oracle verifies each settle window against the config-aware expected
	// state; settleEvery is how often the tick loop pauses for one (0 disables
	// the scheduled settles, leaving only the final settle drive runs).
	// lastActionOffset is the journal offset of the last executed action — the
	// value a settle violation is stamped with. All three are late-bound in
	// drive after the oracle has primed against the start state, matching the
	// ap.jrnl precedent: newEngine's signature is unchanged, and a nil oracle
	// keeps every existing engine test valid.
	oracle           *oracle
	settleEvery      time.Duration
	lastActionOffset int

	// wait blocks for a duration unless ctx is cancelled first. Every wait
	// the tick loop makes goes through it, so a Ctrl-C is observed while
	// the engine is idling out a tick interval or holding a fault instead
	// of only after that wait has run its course.
	wait func(ctx context.Context, d time.Duration) bool
	now  func() time.Time

	// abort ends the tick loop early: set by park, read by run, both on
	// the engine's own goroutine.
	abort bool

	mu    sync.Mutex
	nodes map[string]string

	// vipOwner is the master the port-forward VIP routes currently point
	// at, so a re-point is only issued (and journaled) when it moves.
	vipOwner string
}

func newEngine(l *lab, p *profile, actions []*action, probes probeSource, jrnl *journal, rec *runRecord) *engine {
	e := &engine{
		rng:      rand.New(rand.NewPCG(uint64(rec.Inputs.Seed), 0)),
		lab:      l,
		profile:  p,
		actions:  actions,
		probes:   probes,
		jrnl:     jrnl,
		rec:      rec,
		duration: time.Duration(rec.Inputs.DurationMS) * time.Millisecond,
		tickMin:  time.Duration(rec.Inputs.TickMinMS) * time.Millisecond,
		tickMax:  time.Duration(rec.Inputs.TickMaxMS) * time.Millisecond,
		wait:     waitFor,
		now:      time.Now,
		nodes:    map[string]string{},
	}
	for _, gw := range gatewayNames() {
		e.nodes[gw] = nodeHealthy
	}
	// The control-plane and routing-flap classes target the shared central
	// and upstream nodes; they carry their own lifecycle in the same node
	// map so the guardrails re-target them only once they have converged.
	// healthyNodes() and the agent-alive baseline check iterate the gateways
	// alone, so these entries never confuse them.
	e.nodes[centralNode] = nodeHealthy
	e.nodes[upstreamNode] = nodeHealthy
	return e
}

// waitFor blocks for d, reporting whether the full duration elapsed. It
// returns false the moment ctx is cancelled.
func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (e *engine) nodeState(gw string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.nodes[gw]
}

func (e *engine) setNodeState(gw, state string) {
	e.mu.Lock()
	e.nodes[gw] = state
	e.mu.Unlock()
	e.jrnl.emit(event{Event: evNodeState, Target: gw, State: state})
}

// healthyNodes snapshots the nodes currently in service. The baseline
// checks only hold these to the agent-alive invariant — a node under
// fault is meant to have no agent.
func (e *engine) healthyNodes() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var healthy []string
	for _, gw := range gatewayNames() {
		if e.nodes[gw] == nodeHealthy {
			healthy = append(healthy, gw)
		}
	}
	return healthy
}

func (e *engine) violate(v violationRecord) {
	v.TS = e.now().UTC().Format(time.RFC3339Nano)
	e.mu.Lock()
	e.rec.Violations = append(e.rec.Violations, v)
	e.mu.Unlock()
	e.jrnl.emit(event{
		Event: evViolation, Kind: v.Kind, Tick: v.Tick,
		Action: v.Action, Target: v.Target, Detail: v.Detail,
	})
}

// recordChecks folds what the baseline sweep evaluated into the run
// record, so a reader of summary.json can tell a run that asserted the
// invariants from one that never got to.
func (e *engine) recordChecks(counts checkCounts) {
	e.mu.Lock()
	e.rec.Checks = counts
	e.mu.Unlock()
}

// decision is what one tick draws from the seeded stream, before any
// guardrail is consulted.
type decision struct {
	tick     int
	interval time.Duration
	action   *action
	target   string
	peer     string
	hold     time.Duration
	flip     int
}

// draw takes exactly five values from the stream, in a fixed order:
// interval, action, target, hold, flip. The count is fixed so that a
// guardrail skip cannot shift the stream — the run that skips a decision
// draws the same values afterwards as the run that executed it, which is
// what makes a replay comparable across labs that behaved differently.
// The flip is drawn on every tick even though only config-flip reads it,
// for the same reason.
func (e *engine) draw(tick int) decision {
	d := decision{tick: tick}
	d.interval = e.drawDuration(e.tickMin, e.tickMax)

	total := 0
	for _, a := range e.actions {
		total += a.weight
	}
	if total == 0 {
		// Probe-and-check-only run: no action to pick, so no target, hold
		// or flip to draw either.
		return d
	}
	pick := e.rng.IntN(total)
	for _, a := range e.actions {
		if pick < a.weight {
			d.action = a
			break
		}
		pick -= a.weight
	}
	gateways := gatewayNames()
	d.target = gateways[e.rng.IntN(len(gateways))]
	d.hold = e.drawDuration(d.action.holdMin, d.action.holdMax)
	d.flip = e.rng.IntN(len(flips()))

	// The gateway draw is taken on every tick, whatever the scope, so the
	// stream stays aligned across a registry mixing all the classes. A
	// central- or upstream-scoped action then discards it and targets the
	// shared node instead; a pair action keeps it as the primary target and
	// derives the ring-next gateway as its peer.
	switch d.action.scope {
	case scopeCentral:
		d.target = centralNode
	case scopeUpstream:
		d.target = upstreamNode
	case scopeGatewayPair:
		d.peer = nextGateway(d.target)
	}
	return d
}

// nextGateway is the ring-next gateway after gw, in gatewayNames() order —
// the peer a gateway-pair fault kills while its primary target drains. It
// resolves the peer identically for the engine and the inject.
func nextGateway(gw string) string {
	names := gatewayNames()
	for i, name := range names {
		if name == gw {
			return names[(i+1)%len(names)]
		}
	}
	return gw
}

// drawDuration takes one value from the stream even when the bounds
// collapse, so the draw count per action never depends on its bounds.
func (e *engine) drawDuration(low, high time.Duration) time.Duration {
	span := high - low
	if span < 0 {
		span = 0
	}
	return low + time.Duration(e.rng.Int64N(int64(span)+1))
}

// guardrails decides whether a drawn decision may execute. They keep a
// run meaningful: the fault target must have returned and converged since
// it was last hit; a fault meaning nothing on the target's current state
// is skipped rather than injected; and a gateway fault always leaves the
// lab somewhere to fail over to. A central- or upstream-scoped fault needs
// no healthy gateway peer — pausing the database or the upstream BGP does
// not depend on how many gateways are up.
func (e *engine) guardrails(ctx context.Context, d decision) string {
	if d.action == nil {
		return skipNoWeightedAction
	}
	if e.nodeState(d.target) != nodeHealthy {
		return skipTargetNotHealthy
	}
	// A fault that changes nothing on what the target is currently running —
	// a masquerade flip on a gateway with no VIP, a route drop on a gateway
	// that carries no such route — would rewrite the same state, or delete
	// nothing, and restart or reconcile the node for it.
	if d.action.applicable != nil && !d.action.applicable(ctx, d.target, d.flip) {
		if d.action.usesFlip {
			return skipFlipNotApplicable
		}
		return skipNotApplicable
	}
	if d.action.scope == scopeGatewayPair && e.nodeState(d.peer) != nodeHealthy {
		return skipPeerNotHealthy
	}
	if d.action.scope == scopeGateway || d.action.scope == scopeGatewayPair {
		if !e.hasHealthyPeer(d) {
			return skipNoHealthyPeer
		}
	}
	return ""
}

// hasHealthyPeer reports whether a gateway besides the fault's own targets
// is healthy, so the lab always has a chassis to fail over to. A pair
// fault holds two gateways down, so it needs a third.
func (e *engine) hasHealthyPeer(d decision) bool {
	for _, gw := range e.healthyNodes() {
		if gw == d.target || gw == d.peer {
			continue
		}
		return true
	}
	return false
}

// run is the tick loop: draw, wait, check the guardrails, execute,
// record — until the duration expires. After the deadline no further
// fault is injected; an action already in flight runs to completion.
func (e *engine) run(ctx context.Context) {
	deadline := e.now().Add(e.duration)
	nextSettle := e.now().Add(e.settleEvery)
	for tick := 1; e.now().Before(deadline); tick++ {
		// A settle window runs between ticks — after the previous action's
		// full execute/converge cycle and before the next draw — so it never
		// lands between an inject and its restore. It draws nothing from the
		// rng, so the decision stream is untouched; it only consumes
		// wall-clock, which is why it is scheduled off the clock and not the
		// tick count.
		if e.oracle != nil && e.settleEvery > 0 && !e.now().Before(nextSettle) {
			e.settle(ctx)
			nextSettle = e.now().Add(e.settleEvery)
		}
		d := e.draw(tick)
		if !e.wait(ctx, d.interval) || !e.now().Before(deadline) {
			return
		}

		e.rec.Ticks = tick
		e.rec.Decisions.Total++
		skip := e.guardrails(ctx, d)
		ev := event{
			Event:      evDecision,
			Tick:       tick,
			IntervalMS: d.interval.Milliseconds(),
			Executed:   boolPtr(skip == ""),
			SkipReason: skip,
		}
		if d.action != nil {
			ev.Action = d.action.name
			ev.Target = d.target
			ev.Peer = d.peer
			ev.Object = d.action.object
			ev.HoldMS = d.hold.Milliseconds()
			// A flip-aware action names the drawn flip, so a decision that
			// was skipped still says which one it would have been.
			if d.action.usesFlip {
				ev.Flip = flipName(d.flip)
			}
		}
		e.jrnl.emit(ev)

		if skip != "" {
			e.rec.Decisions.Skipped++
			continue
		}
		e.rec.Decisions.Executed++
		e.rec.ActionsByName[d.action.name]++
		e.execute(ctx, d)
		if e.abort {
			return
		}
	}
}

// settle runs one settle window against the config-aware oracle: it polls
// the lab until every gateway's live data plane matches the state its
// configuration demands or the oracle's settleTimeout expires, records the
// verdict, and stamps each violation the oracle returns with the current
// tick and the journal offset of the last executed action — so a reader
// jumps from a settle violation to the fault that preceded it. drive runs
// it once more after the last tick; the tick loop runs it on a cadence.
func (e *engine) settle(ctx context.Context) {
	e.jrnl.emit(event{Event: evSettleStart, Tick: e.rec.Ticks})

	violations, convergedMS := e.oracle.verify(ctx)
	for _, v := range violations {
		v.Tick = e.rec.Ticks
		v.JournalOffset = e.lastActionOffset
		e.violate(v)
	}
	e.rec.Settles = append(e.rec.Settles, settleRecord{
		Tick:        e.rec.Ticks,
		ConvergedMS: convergedMS,
		Passed:      len(violations) == 0,
		Violations:  len(violations),
	})

	result := resultPass
	if len(violations) > 0 {
		result = resultFail
	}
	ev := event{Event: evSettleResult, Tick: e.rec.Ticks, Result: result}
	// convergedMS is -1 on a deadline; only a real convergence time is worth
	// recording (the field is omitempty).
	if convergedMS >= 0 {
		ev.ConvergedMS = convergedMS
	}
	e.jrnl.emit(ev)
}

// nodesFor is the set of nodes one decision disrupts: its target, plus the
// ring-next peer for a gateway-pair fault. It is what execute marks
// disrupted, what the restore loop puts back, and what convergence gates
// on — so a two-gateway fault is tracked as one unit.
func (e *engine) nodesFor(d decision) []string {
	if d.action.scope == scopeGatewayPair {
		return []string{d.target, d.peer}
	}
	return []string{d.target}
}

// execute injects the fault, holds it, restores each node it disrupted,
// then gates on convergence before the nodes are eligible again.
func (e *engine) execute(ctx context.Context, d decision) {
	nodes := e.nodesFor(d)
	for _, n := range nodes {
		e.setNodeState(n, nodeDisrupted)
	}

	injectedAt := e.now()
	e.jrnl.emit(event{Event: evInject, Tick: d.tick, Action: d.action.name, Target: d.target, Peer: d.peer})
	e.lastActionOffset = e.jrnl.count()

	// Ask the oracle to record what this fault means for the drain
	// classification before it lands. An error means the question could not
	// be asked (docker under load, the container gone mid-inject); the oracle
	// tolerates the residue rather than fabricating a violation, and here it
	// is journaled — never fatal, mirroring checkError's "could not ask"
	// philosophy.
	if e.oracle != nil {
		if err := e.oracle.observeInject(ctx, d.action.name, d.target); err != nil {
			e.jrnl.emit(event{Event: evCheckError, Tick: d.tick, Target: d.target,
				Detail: "oracle drain classification: " + err.Error()})
		}
	}

	if err := d.action.inject(ctx, e.lab, d.target, d.flip); err != nil {
		e.undo(ctx, d, err)
		return
	}

	// The hold is interruptible, but a cancelled run still has to undo the
	// fault it is holding — so it falls through to the restore below
	// rather than returning here.
	e.wait(ctx, d.hold)

	// An injected fault must be undone even when the run is cancelled, or
	// the lab is left with a dead gateway whose restart policy is off and
	// no scenario after it can run. A cancelled ctx would kill every docker
	// invocation on sight, and the restore is long enough to be interrupted
	// half-way through — startAndRestoreGateway waits out two daemon
	// bring-ups and pushes BGP — so every node's restore runs on a context
	// of its own, detached from the signal and bounded by restoreTimeout.
	// The bound is per node, not per action: a double failover restores two
	// gateways, and one 5-minute budget shared across both would cut the
	// second bring-up off half-way.
	for _, n := range nodes {
		e.jrnl.emit(event{Event: evRestore, Tick: d.tick, Action: d.action.name, Target: n})
		if err := e.restoreNode(ctx, d, n); err != nil {
			e.failAction(d, "restore", n, err)
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	restoredAt := e.now()
	for _, n := range nodes {
		e.setNodeState(n, nodeConverging)
	}

	e.converge(ctx, d, injectedAt, restoredAt)
}

// restoreNode runs one node's restore on its own detached, bounded
// context.
func (e *engine) restoreNode(ctx context.Context, d decision, node string) error {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	return d.action.restore(restoreCtx, e.lab, node)
}

// undo restores a fault whose inject failed half-way, then fails the
// action.
//
// An inject is not atomic: injectGatewayKill and injectAgentTerminate
// both pin the docker restart policy to "no" before the step that can
// fail, so bailing out on the error would hand the lab a gateway docker
// will never revive — with its containerlab veth to `upstream` gone for
// good — and only `make e2e-down && make e2e-up` gets it back. The
// restore is the one path that puts the policy back (and re-wires the
// underlay), so it runs even here: on the same detached, bounded context
// the held-fault restore uses, since the inject may well have failed
// because the run was cancelled underneath it.
func (e *engine) undo(ctx context.Context, d decision, injectErr error) {
	for _, n := range e.nodesFor(d) {
		detail := "undo after a failed inject"
		if err := e.restoreNode(ctx, d, n); err != nil {
			detail += ": " + err.Error()
		}
		e.jrnl.emit(event{
			Event: evRestore, Tick: d.tick, Action: d.action.name,
			Target: n, Detail: detail,
		})
	}
	e.failAction(d, "inject", d.target, injectErr)
}

// failAction parks the failing node: a fault the runner could not inject or
// undo leaves the lab in a state it cannot reason about, so the node is
// never targeted again and the run fails.
func (e *engine) failAction(d decision, phase, node string, err error) {
	e.violate(violationRecord{
		Kind: violationActionFailed, Tick: d.tick,
		Action: d.action.name, Target: node,
		Detail: fmt.Sprintf("%s: %v", phase, err),
	})
	e.park(node)
}

// park takes a node out of the run for good and stops the tick loop.
//
// Whatever the parked node was carrying stays dark for the rest of the
// run — it is never re-targeted and never re-healed — and converged()
// gates on *every* probe being green. So no later action against any
// other gateway could converge either: each one would burn its full
// recovery budget on a condition that can no longer be satisfied, park
// its own target, and fill the record with violations derived from this
// one. The violation that got here has already failed the run; stopping
// leaves an artifact bundle that shows the original fault instead of the
// cascade.
func (e *engine) park(gw string) {
	e.setNodeState(gw, nodeUnconverged)
	e.abort = true
	e.jrnl.emit(event{
		Event: evRunAborted, Target: gw,
		Detail: "node parked: no later action could converge with its data path down",
	})
}

// converge polls the restored node back to health within the action's
// recovery budget: the container healthy, the chassis back in SB, the
// VIP routes following the current master, and every probe target green.
// Budget expiry is the reachability-recovery violation the run asserts
// against.
func (e *engine) converge(ctx context.Context, d decision, injectedAt, restoredAt time.Time) {
	nodes := e.nodesFor(d)
	deadline := restoredAt.Add(d.action.recoveryBudget)
	for e.now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		if e.converged(ctx, d) {
			for _, n := range nodes {
				e.setNodeState(n, nodeHealthy)
			}
			fromRestore := e.probes.recoverySince(restoredAt)
			e.rec.Recoveries = append(e.rec.Recoveries, recoveryRecord{
				Tick:          d.tick,
				Action:        d.action.name,
				Target:        d.target,
				Peer:          d.peer,
				BudgetMS:      d.action.recoveryBudget.Milliseconds(),
				ConvergedMS:   e.now().Sub(restoredAt).Milliseconds(),
				FromInjectMS:  e.probes.recoverySince(injectedAt),
				FromRestoreMS: fromRestore,
				CROwnerAfter:  e.vipOwner,
			})
			e.jrnl.emit(event{
				Event: evConverged, Tick: d.tick, Action: d.action.name,
				Target: d.target, Peer: d.peer, CROwner: e.vipOwner, RecoveryMS: fromRestore,
			})
			return
		}
		e.wait(ctx, convergePollInterval)
	}
	e.violate(violationRecord{
		Kind: violationRecoveryTimeout, Tick: d.tick,
		Action: d.action.name, Target: d.target,
		Detail: fmt.Sprintf("not converged within %s; red probes: %s",
			d.action.recoveryBudget, strings.Join(e.probes.redTargets(), ",")),
	})
	e.park(d.target)
}

// converged reports whether every node the decision disrupted is back in
// service and the data path is green again. Each node is checked by the
// signal its scope defines — a gateway's container health and chassis, the
// central databases answering, the upstream BGP daemon back up — and the
// probes are consulted once, after every node has returned.
func (e *engine) converged(ctx context.Context, d decision) bool {
	for _, n := range e.nodesFor(d) {
		if !e.nodeConverged(ctx, d, n) {
			return false
		}
	}
	e.followMaster(ctx)
	return e.probes.allGreen()
}

// nodeConverged reports whether one node is back in service, by the signal
// its scope defines.
func (e *engine) nodeConverged(ctx context.Context, d decision, node string) bool {
	switch d.action.scope {
	case scopeCentral:
		// The central image's HEALTHCHECK is `ovn-nbctl show && ovn-sbctl
		// show`, so a healthy container is exactly both databases answering.
		return e.lab.containerHealth(ctx, node) == "healthy"
	case scopeUpstream:
		return bgpdAlive(ctx, e.lab)
	default:
		return e.lab.containerHealth(ctx, node) == "healthy" && e.lab.chassisInSB(ctx, node)
	}
}

// bgpdAlive reports whether the upstream BGP daemon is running — the
// convergence signal for the routing-flap class, whose faults stop bgpd on
// the upstream node. pgrep exits 1 when nothing matches, which is the fault
// still in place; any other failure is a question that could not be asked,
// and reads here as "not yet back" so convergence keeps polling.
func bgpdAlive(ctx context.Context, l *lab) bool {
	out, err := l.exec(ctx, upstreamNode, "pgrep", "-x", "bgpd")
	return err == nil && strings.TrimSpace(out) != ""
}

// followMaster keeps the port-forward VIP's hand-plumbed routes pointed
// at whichever chassis currently owns cr-lr0-public. Re-election is the
// whole point of the fault set, and the agent does not manage
// Load_Balancer VIP routes — so without this the VIP probe would stay
// red for the rest of the run after the first migration.
//
// A profile without the port-forward layer has no Load_Balancer VIP at
// all: there are no routes to re-point, and issuing them would plumb a
// VIP the run never put up.
func (e *engine) followMaster(ctx context.Context) {
	if !e.profile.ovnLB {
		return
	}
	master := e.lab.currentMaster(ctx)
	if master == "" || master == e.vipOwner {
		return
	}
	if err := e.lab.ensureVIPRouting(ctx, master); err != nil {
		e.jrnl.emit(event{Event: evVIPRepoint, Target: master, Detail: err.Error()})
		return
	}
	e.vipOwner = master
	e.jrnl.emit(event{Event: evVIPRepoint, Target: master})
}

// parseWeights parses the `-weights name=n,...` flag against the action
// registry, rejecting names the registry does not know. Unnamed actions
// keep their default weight.
func parseWeights(spec string, actions []*action) (map[string]int, error) {
	weights := map[string]int{}
	for _, a := range actions {
		weights[a.name] = a.weight
	}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("weight %q is not name=value", pair)
		}
		name = strings.TrimSpace(name)
		if _, known := weights[name]; !known {
			return nil, fmt.Errorf("unknown action %q in -weights (known: %s)",
				name, strings.Join(actionNames(actions), ", "))
		}
		weight, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("weight for %q: %w", name, err)
		}
		if weight < 0 {
			return nil, fmt.Errorf("weight for %q is negative", name)
		}
		weights[name] = weight
	}
	return weights, nil
}

func actionNames(actions []*action) []string {
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		names = append(names, a.name)
	}
	sort.Strings(names)
	return names
}

// applyWeights overrides the registry's default weights in place. The
// registry order is untouched: the weighted pick walks it in order, so
// the order is part of the replay contract.
func applyWeights(actions []*action, weights map[string]int) {
	for _, a := range actions {
		if w, ok := weights[a.name]; ok {
			a.weight = w
		}
	}
}
