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
	e := newEngine(newTestLab(cmd, clock), actions, greenProbes{}, jrnl, rec)
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
	act := noopActions("controller-restart")[0]

	tests := []struct {
		name  string
		nodes map[string]string
		want  string
	}{
		{
			name:  "all nodes healthy",
			nodes: map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:  "",
		},
		{
			name:  "target still converging from an earlier fault",
			nodes: map[string]string{"gateway-1": nodeConverging, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:  skipTargetNotHealthy,
		},
		{
			name:  "target never came back",
			nodes: map[string]string{"gateway-1": nodeUnconverged, "gateway-2": nodeHealthy, "gateway-3": nodeHealthy},
			want:  skipTargetNotHealthy,
		},
		{
			name:  "target is the last healthy gateway",
			nodes: map[string]string{"gateway-1": nodeHealthy, "gateway-2": nodeUnconverged, "gateway-3": nodeUnconverged},
			want:  skipNoHealthyPeer,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newFakeClock()
			e := newEngine(newTestLab(&fakeCommander{}, clock), []*action{act},
				greenProbes{}, newJournal(&bytes.Buffer{}, clock.now),
				&runRecord{ActionsByName: map[string]int{}})
			e.nodes = tc.nodes

			got := e.guardrails(decision{action: act, target: "gateway-1"})
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
		actions, greenProbes{}, newJournal(&buf, clock.now), rec)
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
	actions[0].inject = func(context.Context, *lab, string) error {
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
		actions, greenProbes{}, newJournal(&bytes.Buffer{}, clock.now), rec)
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
		actions, greenProbes{}, newJournal(&bytes.Buffer{}, clock.now), rec)
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
	e := newEngine(newTestLab(cmd, clock), nil, greenProbes{},
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
	actions[0].inject = func(context.Context, *lab, string) error {
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
	actions[0].inject = func(context.Context, *lab, string) error { return errBoom }
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
	e := newEngine(newTestLab(cmd, clock), []*action{actionNamed(t, "agent-terminate")},
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
		noopActions("controller-restart"), greenProbes{},
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
	e := newEngine(newTestLab(cmd, clock), actions, greenProbes{}, newJournal(&buf, clock.now), rec)
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
