package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// newTestChurner wires a churner against the fake commander, with a journal
// pointed at a buffer so a test can read the ovn-churn events it emits.
func newTestChurner(cmd commander) (*churner, *bytes.Buffer) {
	clock := newFakeClock()
	ch := newChurner(newTestLab(cmd, clock))
	var buf bytes.Buffer
	ch.jrnl = newJournal(&buf, clock.now)
	return ch, &buf
}

func churnActionNamed(t *testing.T, p *profile, ch *churner, name string) *action {
	t.Helper()
	for _, a := range churnActions(p, ch) {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no churn action named %q", name)
	return nil
}

func churnEventsIn(t *testing.T, journal string) []event {
	t.Helper()
	var out []event
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evOVNChurn {
			out = append(out, ev)
		}
	}
	return out
}

// fip-churn adds the spare FIP when the NAT row is absent and removes it
// when present, and journals each move.
func TestFIPChurnTogglesTheNATRow(t *testing.T) {
	present := false
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "find NAT external_ip=192.0.2.60") {
			if present {
				return "some-nat-uuid\n", nil
			}
			return "\n", nil
		}
		return healthyLabResponses(argv)
	}}
	ch, buf := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "fip-churn")

	if err := act.inject(context.Background(), ch.lab, centralNode, 0); err != nil {
		t.Fatalf("inject (absent): %v", err)
	}
	if !cmd.called("lr-nat-add lr0 dnat_and_snat 192.0.2.60 192.168.10.60") {
		t.Fatalf("the churn did not add the FIP: %v", cmd.lines())
	}

	present = true
	if err := act.inject(context.Background(), ch.lab, centralNode, 0); err != nil {
		t.Fatalf("inject (present): %v", err)
	}
	if !cmd.called("lr-nat-del lr0 dnat_and_snat 192.0.2.60") {
		t.Fatalf("the churn did not remove the FIP: %v", cmd.lines())
	}

	churns := churnEventsIn(t, buf.String())
	if len(churns) != 2 {
		t.Fatalf("emitted %d ovn-churn events, want 2: %+v", len(churns), churns)
	}
	if churns[0].From != "absent" || churns[0].To != churnFIP {
		t.Fatalf("the add churn did not move absent→%s: %+v", churnFIP, churns[0])
	}
	if churns[1].From != churnFIP || churns[1].To != "absent" {
		t.Fatalf("the remove churn did not move %s→absent: %+v", churnFIP, churns[1])
	}
}

// A NAT lookup the runner cannot answer must fail the churn rather than
// guess which way to toggle.
func TestFIPChurnReportsANBFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "find NAT external_ip=192.0.2.60") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	ch, _ := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "fip-churn")

	if err := act.inject(context.Background(), ch.lab, centralNode, 0); err == nil {
		t.Fatal("a failed NAT lookup was swallowed")
	}
}

// lb-vip-churn only means anything on a profile that put up the port-forward
// Load_Balancer.
func TestLBVIPChurnAppliesOnlyWithTheLoadBalancer(t *testing.T) {
	ch, _ := newTestChurner(&fakeCommander{respond: healthyLabResponses})

	withLB := churnActionNamed(t, defaultTestProfile(t), ch, "lb-vip-churn")
	if !withLB.applicable(context.Background(), centralNode, 0) {
		t.Fatal("lb-vip-churn is inapplicable on a profile with the Load_Balancer")
	}
	withoutLB := churnActionNamed(t, testProfile(t, "pf-only"), ch, "lb-vip-churn")
	if withoutLB.applicable(context.Background(), centralNode, 0) {
		t.Fatal("lb-vip-churn is applicable on a profile with no Load_Balancer")
	}
}

// lb-vip-churn sets a second vips entry when it is absent and removes it
// when present.
func TestLBVIPChurnTogglesTheVIPSEntry(t *testing.T) {
	hasVIP := false
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "find Load_Balancer name=pf-external") {
			if hasVIP {
				return "{192.0.2.50:80=192.168.10.10:8080, 192.0.2.50:81=192.168.10.10:8080}\n", nil
			}
			return "{192.0.2.50:80=192.168.10.10:8080}\n", nil
		}
		return healthyLabResponses(argv)
	}}
	ch, _ := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "lb-vip-churn")

	if err := act.inject(context.Background(), ch.lab, centralNode, 0); err != nil {
		t.Fatalf("inject (absent): %v", err)
	}
	if !cmd.called(`set load_balancer pf-external vips:"192.0.2.50:81"="192.168.10.10:8080"`) {
		t.Fatalf("the churn did not add the vips entry: %v", cmd.lines())
	}

	hasVIP = true
	if err := act.inject(context.Background(), ch.lab, centralNode, 0); err != nil {
		t.Fatalf("inject (present): %v", err)
	}
	// The key carries its double quotes into the argv: an OVSDB string atom
	// holding a colon is unparsable bare, and nbctl gets no shell to add them.
	if !cmd.called(`remove load_balancer pf-external vips "192.0.2.50:81"`) {
		t.Fatalf("the churn did not remove the vips entry: %v", cmd.lines())
	}
}

// A vips lookup the runner cannot answer must fail the churn rather than
// guess which way to toggle, exactly as the FIP churn's does.
func TestLBVIPChurnReportsAFailedLookup(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "find Load_Balancer name=pf-external") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	ch, _ := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "lb-vip-churn")

	err := act.inject(context.Background(), ch.lab, centralNode, 0)
	if err == nil {
		t.Fatal("a failed vips lookup was swallowed")
	}
	if !strings.Contains(err.Error(), "look up the port-forward Load_Balancer vips") {
		t.Fatalf("error = %v, want it wrapped as the vips lookup", err)
	}
}

// A remove the runner cannot issue must surface as an error — this is the
// path a malformed command takes, and the engine turns it into the
// action-failed violation that says the harness, not the agent, failed.
func TestLBVIPChurnReportsAFailedRemove(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "find Load_Balancer name=pf-external"):
			return "{192.0.2.50:80=192.168.10.10:8080, 192.0.2.50:81=192.168.10.10:8080}\n", nil
		case strings.Contains(line, "remove load_balancer"):
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	ch, _ := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "lb-vip-churn")

	err := act.inject(context.Background(), ch.lab, centralNode, 0)
	if err == nil {
		t.Fatal("a failed vips remove was swallowed")
	}
	if !strings.Contains(err.Error(), "remove the churn VIP entry") {
		t.Fatalf("error = %v, want it wrapped as the churn VIP removal", err)
	}
}

// An empty vips column is the absent case: the Load_Balancer exists with no
// entries at all, so the churn adds rather than removes.
func TestLBVIPChurnAddsWhenTheVIPSColumnIsEmpty(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "find Load_Balancer name=pf-external") {
			return "", nil
		}
		return healthyLabResponses(argv)
	}}
	ch, buf := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "lb-vip-churn")

	if err := act.inject(context.Background(), ch.lab, centralNode, 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !cmd.called(`set load_balancer pf-external vips:"192.0.2.50:81"="192.168.10.10:8080"`) {
		t.Fatalf("an empty vips column did not take the add branch: %v", cmd.lines())
	}
	churns := churnEventsIn(t, buf.String())
	if len(churns) != 1 || churns[0].From != "absent" || churns[0].To != "present" {
		t.Fatalf("the churn did not journal absent→present: %+v", churns)
	}
}

// priority-flip reads the current Gateway_Chassis priorities and bumps the
// target ten above the peak, exactly as the bootstrap master-convergence
// mechanic does.
func TestPriorityFlipBumpsAboveThePeak(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "--columns=priority list Gateway_Chassis") {
			return "30\n20\n10\n", nil
		}
		return healthyLabResponses(argv)
	}}
	ch, buf := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "priority-flip")

	if err := act.inject(context.Background(), ch.lab, "gateway-2", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !cmd.called("lrp-set-gateway-chassis lr0-public gateway-2 40") {
		t.Fatalf("priority-flip did not bump ten above the peak (30): %v", cmd.lines())
	}

	churns := churnEventsIn(t, buf.String())
	if len(churns) != 1 || churns[0].From != "30" || churns[0].To != "40" {
		t.Fatalf("priority-flip did not journal 30→40: %+v", churns)
	}
}

// chassis-delete removes the target's SB Chassis row with --if-exists and
// restores nothing — the live ovn-controller re-registers it within the
// grace period.
func TestChassisDeleteRemovesTheRowAndRestoresNothing(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	ch, _ := newTestChurner(cmd)
	act := churnActionNamed(t, defaultTestProfile(t), ch, "chassis-delete")

	if err := act.inject(context.Background(), ch.lab, "gateway-3", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !cmd.called("--if-exists chassis-del gateway-3") {
		t.Fatalf("chassis-delete did not remove the row with --if-exists: %v", cmd.lines())
	}

	restoreCmd := &fakeCommander{respond: healthyLabResponses}
	rl := newTestLab(restoreCmd, newFakeClock())
	if err := act.restore(context.Background(), rl, "gateway-3"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(restoreCmd.lines()) != 0 {
		t.Fatalf("chassis-delete restore issued commands, want none: %v", restoreCmd.lines())
	}
}
