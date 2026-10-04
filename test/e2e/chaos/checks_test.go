package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// newTestChecks wires the baseline checks against a fake lab and an
// engine whose node states the test controls, and hands back the run
// record and the journal both write into.
func newTestChecks(t *testing.T, cmd commander, nodes map[string]string) (*baselineChecks, *runRecord, *bytes.Buffer) {
	t.Helper()
	clock := newFakeClock()
	rec := &runRecord{ActionsByName: map[string]int{}}
	buf := &bytes.Buffer{}
	e := newEngine(newTestLab(cmd, clock), defaultTestProfile(t), nil, greenProbes{}, newJournal(buf, clock.now), rec)
	e.now = clock.now
	if nodes != nil {
		e.nodes = nodes
	}
	return &baselineChecks{lab: e.lab, engine: e}, rec, buf
}

// holdFault opens the span of a fault on central at tick 4, as execute
// does right before the inject, and leaves it held.
func holdFault(c *baselineChecks, name string) int {
	return c.engine.beginHold(decision{tick: 4, action: &action{name: name}, target: centralNode})
}

// lookupFails answers the dual-claim lookup with errBoom and everything
// else the way a healthy lab does.
func lookupFails(argv []string) (string, error) {
	if strings.Contains(strings.Join(argv, " "), "type=chassisredirect") {
		return "", errBoom
	}
	return healthyLabResponses(argv)
}

// eventsNamed is the journal's events of one name, in order.
func eventsNamed(t *testing.T, journal, name string) []event {
	t.Helper()
	var out []event
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == name {
			out = append(out, ev)
		}
	}
	return out
}

// The agent staying alive is one of the run's invariants: a node the run
// considers healthy but that has no agent process is a violation.
func TestCheckAgentsAliveFlagsAHealthyNodeWithNoAgent(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		// pgrep finds nothing on gateway-2: it exits 1, and that is the
		// answer, not a failure.
		if strings.Contains(line, "pgrep") && strings.Contains(line, "gateway-2") {
			return "", errExit(t, 1)
		}
		if strings.Contains(line, "pgrep") {
			return "1\n", nil
		}
		return healthyLabResponses(argv)
	}}
	checks, rec, _ := newTestChecks(t, cmd, nil)

	checks.checkAgentsAlive(context.Background())

	if len(rec.Violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", rec.Violations)
	}
	v := rec.Violations[0]
	if v.Kind != violationAgentDown || v.Target != "gateway-2" {
		t.Fatalf("violation = %+v, want %s on gateway-2", v, violationAgentDown)
	}
}

// A probe that could not be answered is not a dead agent. The runner
// drives docker hard — the prober fires five execs a second while the
// engine is issuing `docker update`, `kill`, `start` and `inspect` — so a
// `docker exec` on a loaded runner can fail for reasons that have nothing
// to do with the agent. Read as "no agent" it would fail the run against a
// node that is perfectly healthy, and point the report at a bug in the
// agent that never happened.
func TestCheckAgentsAliveDoesNotReadAFailedProbeAsADeadAgent(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pgrep") {
			return "", errBoom // the docker daemon could not answer
		}
		return healthyLabResponses(argv)
	}}
	checks, rec, buf := newTestChecks(t, cmd, nil)

	checks.checkAgentsAlive(context.Background())

	if len(rec.Violations) != 0 {
		t.Fatalf("a probe the runner could not answer was recorded as a violation: %+v", rec.Violations)
	}
	if checks.counts.Errors != len(gatewayNames()) {
		t.Fatalf("counted %d check errors for %d unanswerable probes",
			checks.counts.Errors, len(gatewayNames()))
	}
	var journaled int
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event == evCheckError && ev.Kind == violationAgentDown {
			journaled++
		}
	}
	if journaled == 0 {
		t.Fatalf("the unanswerable probe was swallowed instead of journaled: %q", buf.String())
	}
}

// A node under fault is *meant* to have no agent — that is the fault.
// Holding it to the invariant would report every injected fault as a
// violation.
func TestCheckAgentsAliveSkipsNodesUnderFault(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pgrep") {
			return "", errBoom // no agent anywhere
		}
		return healthyLabResponses(argv)
	}}
	checks, rec, _ := newTestChecks(t, cmd, map[string]string{
		"gateway-1": nodeDisrupted,
		"gateway-2": nodeConverging,
		"gateway-3": nodeUnconverged,
	})

	checks.checkAgentsAlive(context.Background())

	if len(rec.Violations) != 0 {
		t.Fatalf("violations = %+v, want none — every node is under fault", rec.Violations)
	}
	if cmd.called("pgrep") {
		t.Fatalf("the check probed a node that is not in service: %v", cmd.lines())
	}
}

// A restarted gateway is booting, not broken. Its container answers
// "healthy" as soon as OVS and ovn-controller are up and its chassis row
// never went away — but the entrypoint still has FRR to bring up before it
// execs the agent, and the run must not put the node back in the healthy
// pool for that whole window. It did, and every sweep in between recorded
// an `agent-down` against a node on its way back: eight of them from one
// config-flip restart in the 2026-07-20 nightly, 33 across four runs
// (issue #205).
func TestARestartedGatewayIsNotSweptUntilItsAgentIsBack(t *testing.T) {
	clock := newFakeClock()
	// The entrypoint execs the agent a minute after the restart. Everything
	// else about the node reads healthy from the first poll — which is
	// exactly what makes the window invisible without asking for the agent.
	agentBackAt := clock.now().Add(60 * time.Second)
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if strings.Contains(line, "pgrep -f "+agentBinary) &&
			strings.Contains(line, "gateway-2") && clock.now().Before(agentBackAt) {
			return "", errExit(t, 1)
		}
		return healthyLabResponses(argv)
	}}

	rec := &runRecord{ActionsByName: map[string]int{}}
	l := newTestLab(cmd, clock)
	e := newEngine(l, defaultTestProfile(t), nil, greenProbes{},
		newJournal(&bytes.Buffer{}, clock.now), rec)
	e.now, e.wait = clock.now, clock.wait
	checks := &baselineChecks{lab: l, engine: e}

	restart := noopActions("config-flip")[0]
	restart.holdMin, restart.holdMax = 0, 0
	restart.recoveryBudget = 180 * time.Second
	e.execute(context.Background(), decision{tick: 1, action: restart, target: "gateway-2"})

	// The sweep the baseline checks run alongside the engine, the moment it
	// declared the node back in service.
	checks.checkAgentsAlive(context.Background())

	if len(rec.Violations) != 0 {
		t.Fatalf("violations = %+v, want none: gateway-2 was booting, not agent-down", rec.Violations)
	}
	if got := e.nodeState("gateway-2"); got != nodeHealthy {
		t.Fatalf("gateway-2 = %q, want %q — its agent came back well inside the budget", got, nodeHealthy)
	}
	if clock.now().Before(agentBackAt) {
		t.Fatalf("convergence returned at %s, before the agent was back at %s: "+
			"the node re-entered the healthy pool while it was still booting",
			clock.now(), agentBackAt)
	}
}

// Two chassis forwarding for the same gateway port at once is the split
// HA re-election exists to avoid. The claim set is the `chassis` column
// plus every member of `additional_chassis`.
func TestCheckNoDualClaimFlagsASplitGatewayPort(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "type=chassisredirect"):
			return `{"headings":["logical_port","chassis","additional_chassis"],
			         "data":[["cr-lr0-public",["uuid","uuid-a"],["set",[["uuid","uuid-b"]]]]]}`, nil
		case strings.Contains(line, "list Chassis uuid-a"):
			return "gateway-1\n", nil
		case strings.Contains(line, "list Chassis uuid-b"):
			return "gateway-2\n", nil
		}
		return "", nil
	}}
	checks, rec, _ := newTestChecks(t, cmd, nil)

	checks.checkNoDualClaim(context.Background())

	if len(rec.Violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", rec.Violations)
	}
	v := rec.Violations[0]
	if v.Kind != violationDualClaim {
		t.Fatalf("violation kind = %q, want %q", v.Kind, violationDualClaim)
	}
	// The detail must name the chassis, not their UUIDs — that is what
	// makes the journal triageable.
	if !strings.Contains(v.Detail, "gateway-1") || !strings.Contains(v.Detail, "gateway-2") {
		t.Fatalf("violation detail %q does not name both claiming chassis", v.Detail)
	}
}

func TestCheckNoDualClaimAcceptsASinglyClaimedPort(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) {
		return `{"headings":["logical_port","chassis","additional_chassis"],
		         "data":[["cr-lr0-public",["uuid","uuid-a"],["set",[]]]]}`, nil
	}}
	checks, rec, _ := newTestChecks(t, cmd, nil)

	checks.checkNoDualClaim(context.Background())

	if len(rec.Violations) != 0 {
		t.Fatalf("violations = %+v, want none — the port has one owner", rec.Violations)
	}
	if checks.counts.DualClaim != 1 {
		t.Fatalf("evaluated the dual-claim invariant %d times, want once", checks.counts.DualClaim)
	}
}

// A sweep that could not query SB checked nothing. That is a runner-side
// problem, not a broken lab invariant — but it must show up in the
// journal rather than pass silently.
func TestCheckNoDualClaimJournalsALookupFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	checks, rec, buf := newTestChecks(t, cmd, nil)

	checks.checkNoDualClaim(context.Background())

	if len(rec.Violations) != 0 {
		t.Fatalf("a failed lookup was recorded as a lab violation: %+v", rec.Violations)
	}
	if checks.counts.DualClaim != 0 {
		t.Fatalf("a failed lookup was counted as an evaluation of the invariant")
	}
	var journaled bool
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event == evCheckError && ev.Kind == violationDualClaim {
			journaled = true
		}
	}
	if !journaled {
		t.Fatalf("the failed lookup was swallowed instead of journaled as a check error: %q", buf.String())
	}
	// No fault was held, so nothing explains the failure: it is an error.
	if checks.counts.Errors != 1 || checks.counts.SkippedUnderFault != 0 {
		t.Fatalf("counts = %+v, want one error and nothing skipped under fault", checks.counts)
	}
	if skipped := eventsNamed(t, buf.String(), evCheckSkipped); len(skipped) != 0 {
		t.Fatalf("a lookup that failed under no fault was journaled as skipped: %+v", skipped)
	}
}

// The run paused the SB itself, so a dual-claim lookup that failed while
// sb-pause held it could not have been answered. It is no sweep breakage:
// it is counted apart from the errors and journaled with the fault that
// explains it. Counted as an error, every sb-pause put one into
// checks.errors per sweep of its hold, 35 across the 2026-10-04 nightly
// (issue #239).
func TestCheckNoDualClaimSkipsALookupThatFailedUnderSBPause(t *testing.T) {
	checks, rec, buf := newTestChecks(t, &fakeCommander{respond: lookupFails}, nil)
	holdFault(checks, "sb-pause")

	checks.checkNoDualClaim(context.Background())

	if got := checks.counts; got.Errors != 0 || got.DualClaim != 0 || got.SkippedUnderFault != 1 {
		t.Fatalf("counts = %+v, want one lookup skipped under fault, no error and no evaluation", got)
	}
	if len(rec.Violations) != 0 {
		t.Fatalf("a lookup the paused SB could not answer was recorded as a violation: %+v", rec.Violations)
	}
	skipped := eventsNamed(t, buf.String(), evCheckSkipped)
	if len(skipped) != 1 {
		t.Fatalf("journaled %d check-skipped events, want one: %q", len(skipped), buf.String())
	}
	ev := skipped[0]
	if ev.Kind != violationDualClaim || ev.Action != "sb-pause" || ev.Target != centralNode ||
		ev.Tick != 4 || ev.Detail != "list chassisredirect port bindings: boom" {
		t.Fatalf("check-skipped = %+v, want the dual-claim lookup's error under tick 4's sb-pause on %s",
			ev, centralNode)
	}
	if errs := eventsNamed(t, buf.String(), evCheckError); len(errs) != 0 {
		t.Fatalf("the skipped lookup was also journaled as a check error: %+v", errs)
	}
}

// unanswerableUnder names its faults by action name, and the tests here
// hold them by the same literal. A name no action carries never matches a
// held span: a renamed sb-pause would turn every lookup under it back into
// a check error while every other test still passed.
func TestUnanswerableUnderNamesActionsTheRunCanDraw(t *testing.T) {
	known := actionNames(fullRegistry(t))
	for kind, faults := range unanswerableUnder {
		for _, name := range faults {
			if !slices.Contains(known, name) {
				t.Errorf("unanswerableUnder[%s] names %q, which is no action (known: %v)", kind, name, known)
			}
		}
	}
}

// sbctl --timeout=5 can hang on the paused SB until after the restore. The
// lookup started inside the hold, so it is still skipped.
func TestCheckNoDualClaimSkipsALookupThatOutlastedTheRestore(t *testing.T) {
	cmd := &fakeCommander{}
	checks, _, _ := newTestChecks(t, cmd, nil)
	i := holdFault(checks, "sb-pause")
	cmd.respond = func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "type=chassisredirect") {
			checks.lab.sleep(time.Second)
			checks.engine.endHold(i) // SIGCONT returns mid-lookup
			checks.lab.sleep(4 * time.Second)
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}

	checks.checkNoDualClaim(context.Background())

	if got := checks.counts; got.SkippedUnderFault != 1 || got.Errors != 0 {
		t.Fatalf("counts = %+v, want the lookup that started under sb-pause skipped, not an error", got)
	}
}

// Once the restore returned the SB answers again. A lookup that started
// after it and failed anyway is a sweep breakage the run did not cause.
func TestCheckNoDualClaimCountsALookupThatFailedAfterTheRestore(t *testing.T) {
	checks, _, buf := newTestChecks(t, &fakeCommander{respond: lookupFails}, nil)
	i := holdFault(checks, "sb-pause")
	checks.lab.sleep(5 * time.Second)
	checks.engine.endHold(i)
	checks.lab.sleep(time.Second)

	checks.checkNoDualClaim(context.Background())

	if got := checks.counts; got.Errors != 1 || got.SkippedUnderFault != 0 {
		t.Fatalf("counts = %+v, want one error and nothing skipped under fault", got)
	}
	if errs := eventsNamed(t, buf.String(), evCheckError); len(errs) != 1 {
		t.Fatalf("journaled %d check errors, want one: %q", len(errs), buf.String())
	}
	if skipped := eventsNamed(t, buf.String(), evCheckSkipped); len(skipped) != 0 {
		t.Fatalf("a lookup after the restore was journaled as skipped: %+v", skipped)
	}
}

// The other faults on central leave the SB answering, so a dual-claim
// lookup that fails under them is exactly the signal checks.errors exists
// for.
func TestCheckNoDualClaimCountsALookupThatFailedUnderAnotherFault(t *testing.T) {
	for _, name := range []string{"nb-pause", "northd-pause", "fip-churn"} {
		t.Run(name, func(t *testing.T) {
			checks, _, _ := newTestChecks(t, &fakeCommander{respond: lookupFails}, nil)
			holdFault(checks, name)

			checks.checkNoDualClaim(context.Background())

			if got := checks.counts; got.Errors != 1 || got.SkippedUnderFault != 0 {
				t.Fatalf("counts = %+v, want one error and nothing skipped under %s", got, name)
			}
		})
	}
}

// pgrep runs on the gateways and never reads the SB, so sb-pause does not
// explain a probe that failed: it stays an error.
func TestCheckAgentsAliveCountsAFailedProbeUnderSBPause(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pgrep") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	checks, _, _ := newTestChecks(t, cmd, nil)
	holdFault(checks, "sb-pause")

	checks.checkAgentsAlive(context.Background())

	if got := checks.counts; got.Errors != len(gatewayNames()) || got.SkippedUnderFault != 0 {
		t.Fatalf("counts = %+v, want %d errors and nothing skipped under fault", got, len(gatewayNames()))
	}
}

// A cancelled run is not a failed check, held fault or not.
func TestACancelledLookupUnderSBPauseIsNeitherCountedNorJournaled(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "type=chassisredirect") {
			return "", context.Canceled
		}
		return healthyLabResponses(argv)
	}}
	checks, _, buf := newTestChecks(t, cmd, nil)
	holdFault(checks, "sb-pause")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	checks.checkNoDualClaim(ctx)

	if got := checks.counts; got.Errors != 0 || got.SkippedUnderFault != 0 {
		t.Fatalf("counts = %+v, want nothing counted for a cancelled run", got)
	}
	for _, name := range []string{evCheckError, evCheckSkipped} {
		if evs := eventsNamed(t, buf.String(), name); len(evs) != 0 {
			t.Fatalf("a cancelled lookup was journaled as %s: %+v", name, evs)
		}
	}
}

// A skipped lookup evaluated nothing. A run whose every lookup was skipped
// under sb-pause asserted nothing, and fails like any other run that never
// got to evaluate the dual-claim invariant.
func TestARunWhoseEveryLookupWasSkippedUnderFaultDoesNotPass(t *testing.T) {
	checks, rec, _ := newTestChecks(t, &fakeCommander{respond: lookupFails}, nil)
	holdFault(checks, "sb-pause")

	checks.sweep(context.Background())
	checks.sweep(context.Background())
	checks.finalize()
	rec.finalize(time.Now())

	if rec.Result != resultFail {
		t.Fatalf("result = %q, want %q — the invariant was never evaluated", rec.Result, resultFail)
	}
	if len(rec.Violations) != 1 || rec.Violations[0].Kind != violationChecksNeverRan {
		t.Fatalf("violations = %+v, want one %s", rec.Violations, violationChecksNeverRan)
	}
	if want := "0 check errors, 2 skipped under a held fault"; !strings.Contains(rec.Violations[0].Detail, want) {
		t.Fatalf("violation detail %q does not say %q", rec.Violations[0].Detail, want)
	}
	if want := (checkCounts{Sweeps: 2, DualClaim: 0, Errors: 0, SkippedUnderFault: 2}); rec.Checks != want {
		t.Fatalf("checks = %+v, want %+v", rec.Checks, want)
	}
}

// The dual-claim invariant is the single most important thing the run
// asserts, and every sweep of it can fail for reasons that have nothing to
// do with the lab. A run where every sweep failed that way has zero
// violations — and used to report a clean pass on that basis, with the
// check-error lines buried in a journal nobody reads on green.
func TestARunThatNeverEvaluatedTheDualClaimDoesNotPass(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "type=chassisredirect") {
			return "", errBoom // central never answers
		}
		return healthyLabResponses(argv)
	}}
	checks, rec, _ := newTestChecks(t, cmd, nil)

	checks.sweep(context.Background())
	checks.sweep(context.Background())
	checks.finalize()
	rec.finalize(time.Now())

	if rec.Result != resultFail {
		t.Fatalf("result = %q, want %q — the invariant was never evaluated", rec.Result, resultFail)
	}
	if len(rec.Violations) != 1 || rec.Violations[0].Kind != violationChecksNeverRan {
		t.Fatalf("violations = %+v, want a %s", rec.Violations, violationChecksNeverRan)
	}
	// The record has to carry the evidence, not just the verdict.
	if rec.Checks.Sweeps != 2 || rec.Checks.DualClaim != 0 || rec.Checks.Errors != 2 {
		t.Fatalf("checks = %+v, want 2 sweeps, 0 evaluations and 2 errors", rec.Checks)
	}
}

// The counterpart: a run whose sweeps did evaluate the invariant and found
// nothing passes, and says how much it looked at.
func TestARunThatEvaluatedTheDualClaimPasses(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "type=chassisredirect") {
			return `{"headings":["logical_port","chassis","additional_chassis"],
			         "data":[["cr-lr0-public",["uuid","uuid-a"],["set",[]]]]}`, nil
		}
		return healthyLabResponses(argv)
	}}
	checks, rec, _ := newTestChecks(t, cmd, nil)

	checks.sweep(context.Background())
	checks.finalize()
	rec.finalize(time.Now())

	if rec.Result != resultPass {
		t.Fatalf("result = %q, want %q: %+v", rec.Result, resultPass, rec.Violations)
	}
	if rec.Checks.Sweeps != 1 || rec.Checks.DualClaim != 1 || rec.Checks.Errors != 0 {
		t.Fatalf("checks = %+v, want 1 sweep, 1 evaluation and no errors", rec.Checks)
	}
}

// drive() cancels the sweep loop and then joins its goroutine. A run that
// did not return on ctx.Done() would hang the runner forever, after the
// engine has already finished.
func TestChecksStopWithTheirContext(t *testing.T) {
	checks, _, _ := newTestChecks(t, &fakeCommander{respond: healthyLabResponses}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checks.run(ctx)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the baseline checks did not stop when their context was cancelled")
	}
}

// The sweep runs both checks.
func TestSweepRunsBothChecks(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	checks, _, _ := newTestChecks(t, cmd, nil)

	checks.sweep(context.Background())

	if !cmd.called("pgrep -f /usr/local/bin/ovn-network-agent") {
		t.Fatalf("the sweep did not check that the agents are alive: %v", cmd.lines())
	}
	if !cmd.called("type=chassisredirect") {
		t.Fatalf("the sweep did not check for a dual claim: %v", cmd.lines())
	}
}
