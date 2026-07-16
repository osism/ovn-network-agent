package main

import (
	"bytes"
	"context"
	"strings"
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
// the lab is left genuinely broken. The node is parked and the run fails.
func TestFailedRestoreParksTheNodeAndFailsTheRun(t *testing.T) {
	actions := noopActions("controller-restart")
	actions[0].restore = func(context.Context, *lab, string) error {
		return errBoom
	}

	journal, rec := runEngine(t, 42, 5*time.Minute, actions, nil)

	if rec.Result != resultFail {
		t.Fatalf("result = %q, want %q", rec.Result, resultFail)
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

// converged() has three gates. The container-health one is driven by
// TestRecoveryBudgetExpiryIsAViolation; these are the other two. Each is
// the only thing between a node that came back wrong — its chassis never
// re-registered, or its data path never came back — and being declared
// healthy and re-targeted.
func TestConvergenceGatesOnTheChassisAndTheDataPath(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*engine)
	}{
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

// The VIP's routes are re-pointed at whichever chassis owns
// cr-lr0-public, and only when it moves — the agent does not manage them,
// so without the re-point the VIP probe goes permanently red on the first
// re-election and every later violation is noise.
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

	e.followMaster(context.Background())

	if e.vipOwner != "gateway-1" {
		t.Fatalf("vipOwner = %q after the first master was seen, want gateway-1", e.vipOwner)
	}
	if got := cmd.count("ip route replace"); got != 2 {
		t.Fatalf("issued %d route commands, want the forward route on upstream and "+
			"the scope-link route on the master: %v", got, cmd.lines())
	}

	// The master has not moved: nothing is issued and nothing is
	// journaled, which is what keeps the journal readable.
	e.followMaster(context.Background())

	if got := cmd.count("ip route replace"); got != 2 {
		t.Fatalf("re-pointed the VIP at a master that never moved: %v", cmd.lines())
	}

	// Re-election, and the re-point fails: the owner must stay where the
	// routes actually point, and the failure must be journaled.
	master, routesFail = "gateway-2", true
	e.followMaster(context.Background())

	if e.vipOwner != "gateway-1" {
		t.Fatalf("vipOwner = %q after a failed re-point, want the routes' real owner gateway-1", e.vipOwner)
	}

	var repoints []event
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event == evVIPRepoint {
			repoints = append(repoints, ev)
		}
	}
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
// reason about: the node is parked, never targeted again, and the run
// fails.
func TestFailedInjectParksTheNodeAndFailsTheRun(t *testing.T) {
	actions := noopActions("controller-restart")
	actions[0].inject = func(context.Context, *lab, string, int) error {
		return errBoom
	}

	_, rec := runEngine(t, 42, 5*time.Minute, actions, nil)

	if rec.Result != resultFail {
		t.Fatalf("result = %q, want %q", rec.Result, resultFail)
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

	e.followMaster(context.Background())

	if len(cmd.lines()) != 0 {
		t.Fatalf("a profile without the port-forward layer still plumbed the VIP: %v", cmd.lines())
	}
	if e.vipOwner != "" {
		t.Fatalf("vipOwner = %q on a run with no Load_Balancer VIP", e.vipOwner)
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
				if strings.Contains(strings.Join(argv, " "), "pgrep -x bgpd") {
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
