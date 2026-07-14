package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// newTestApplier wires an applier against the fake commander, with the
// management addresses already discovered — every test below is about
// what it does with them.
func newTestApplier(t *testing.T, cmd commander, name string) *applier {
	t.Helper()
	a := newApplier(newTestLab(cmd, newFakeClock()), testProfile(t, name), baseConfig(t))
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	return a
}

// restartCmd is the container-qualified form of the restart, because the
// baked config's own comments mention `docker restart` — a bare substring
// match would find them in the argv that *writes* the config.
const restartCmd = "docker restart clab-"

// labWithConfig answers `cat <live config>` with what the gateway is
// running, and everything else the way a healthy lab does.
func labWithConfig(live string) func(argv []string) (string, error) {
	return func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "cat "+agentConfigPath):
			return live, nil
		case strings.Contains(line, "ip -o -4 addr show eth0"):
			return "172.20.20.4\n", nil
		}
		return healthyLabResponses(argv)
	}
}

// The whole point of the two-phase apply: a profile the agent rejects on
// *one* gateway must not leave the lab half-reconfigured, with two
// gateways rolled onto it and the third refusing to start. So every
// gateway is validated before any live file is touched.
func TestApplyProfileValidatesEveryGatewayBeforeItSwapsAnything(t *testing.T) {
	cmd := &fakeCommander{respond: labWithConfig("stale config\n")}
	a := newTestApplier(t, cmd, "flat-dnat")

	if err := a.applyProfile(context.Background(), newJournal(&bytes.Buffer{}, newFakeClock().now)); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}

	if got := cmd.count("--check-config"); got != len(gatewayNames()) {
		t.Fatalf("validated %d of %d gateways", got, len(gatewayNames()))
	}
	firstSwap := cmd.indexOf("mv " + agentConfigNextPath)
	if firstSwap < 0 {
		t.Fatalf("nothing was swapped at all: %v", cmd.lines())
	}
	for i, line := range cmd.lines() {
		if strings.Contains(line, "--check-config") && i > firstSwap {
			t.Fatalf("gateway %d was validated only after another one had been swapped: %v", i, cmd.lines())
		}
	}
	if got := cmd.count(restartCmd); got != len(gatewayNames()) {
		t.Fatalf("restarted %d gateways onto the new config, want all %d", got, len(gatewayNames()))
	}
}

// A config the agent refuses is the run's own fault, not the lab's: the
// run is abandoned before a single live file moves.
func TestApplyProfileAbortsWhenTheAgentRejectsTheConfig(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "--check-config") {
			return "", errExit(t, 1)
		}
		return labWithConfig("stale config\n")(argv)
	}}
	a := newTestApplier(t, cmd, "pf-only")

	err := a.applyProfile(context.Background(), newJournal(&bytes.Buffer{}, newFakeClock().now))

	if err == nil {
		t.Fatal("a configuration the agent rejected was rolled out anyway")
	}
	if !strings.Contains(err.Error(), "rejected") || !strings.Contains(err.Error(), "pf-only") {
		t.Fatalf("error %q names neither the rejection nor the profile", err)
	}
	if cmd.called("mv "+agentConfigNextPath) || cmd.called(restartCmd) {
		t.Fatalf("a rejected profile still touched the live config: %v", cmd.lines())
	}
}

// A check the runner could not *run* is not a rejection — it is a
// question that went unanswered, and swapping a live config on the
// strength of it is exactly what the gate exists to prevent.
func TestApplyProfileAbortsWhenTheCheckCannotRun(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "--check-config") {
			return "", errBoom
		}
		return labWithConfig("stale config\n")(argv)
	}}
	a := newTestApplier(t, cmd, "flat-minimal")

	if err := a.applyProfile(context.Background(), newJournal(&bytes.Buffer{}, newFakeClock().now)); err == nil {
		t.Fatal("a config whose validation never ran was rolled out anyway")
	}
	if cmd.called(restartCmd) {
		t.Fatalf("a gateway was restarted onto an unvalidated config: %v", cmd.lines())
	}
}

// The default profile on a fresh lab renders exactly the bytes the image
// baked in. Restarting three gateways to install the config they are
// already running would cost the run two re-elections and buy nothing.
func TestApplyProfileSkipsGatewaysThatAreAlreadyOnTheConfig(t *testing.T) {
	cmd := &fakeCommander{respond: labWithConfig(string(baseConfig(t)))}
	a := newTestApplier(t, cmd, defaultProfileName)
	var buf bytes.Buffer

	if err := a.applyProfile(context.Background(), newJournal(&buf, newFakeClock().now)); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}

	if cmd.called(restartCmd) || cmd.called("mv "+agentConfigNextPath) {
		t.Fatalf("a gateway already on the config was restarted anyway: %v", cmd.lines())
	}
	// The marker is removed, so the lab's deploy-time drain switch keeps
	// applying to a run that changes no configuration.
	if got := cmd.count("rm -f " + profileMarkerPath); got != len(gatewayNames()) {
		t.Fatalf("removed the profile marker from %d gateways, want all %d", got, len(gatewayNames()))
	}
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event == evProfileApply && (ev.Executed == nil || *ev.Executed) {
			t.Fatalf("journaled %+v, want every gateway reported as unchanged", ev)
		}
	}
}

// The lab has to keep forwarding while it is reconfigured, so the
// gateways roll one at a time: each is back — container healthy, chassis
// re-registered — before the next one is touched.
func TestApplyProfileRollsTheGatewaysOneAtATime(t *testing.T) {
	// gateway-1 is already on the config the heterogeneous profile leaves
	// it on — the baked one — so only the other two have to roll.
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if strings.Contains(line, "cat "+agentConfigPath) && strings.Contains(line, "gateway-1") {
			return string(baseConfig(t)), nil
		}
		return labWithConfig("stale config\n")(argv)
	}}
	a := newTestApplier(t, cmd, "heterogeneous")

	if err := a.applyProfile(context.Background(), newJournal(&bytes.Buffer{}, newFakeClock().now)); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}

	firstRestart := cmd.indexOf("docker restart clab-ovn-e2e-gateway-2")
	firstBack := cmd.indexOf("find Chassis name=gateway-2")
	secondRestart := cmd.indexOf("docker restart clab-ovn-e2e-gateway-3")
	if firstRestart < 0 || firstBack < 0 || secondRestart < 0 {
		t.Fatalf("the roll did not restart and gate both overlaid gateways: %v", cmd.lines())
	}
	if firstBack > secondRestart {
		t.Fatalf("gateway-3 was restarted before gateway-2 was back: %v", cmd.lines())
	}
	// gateway-1 runs the baked config under this profile, so it is never
	// restarted at all.
	if cmd.called("docker restart clab-ovn-e2e-gateway-1") {
		t.Fatalf("a gateway the profile leaves alone was restarted: %v", cmd.lines())
	}
	if !cmd.called("printf '%s' 'heterogeneous' > " + profileMarkerPath) {
		t.Fatalf("the reconfigured gateways were not marked as profile-owned: %v", cmd.lines())
	}
}

// The API VIP forwards to a backend on the gateway's *own* management
// address, so the rendered rule must carry the address that gateway
// actually has — not the one another gateway has.
func TestApplyProfileRendersEachGatewaysOwnBackendAddress(t *testing.T) {
	addrs := map[string]string{
		"gateway-1": "172.20.20.4",
		"gateway-2": "172.20.20.5",
		"gateway-3": "172.20.20.6",
	}
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if strings.Contains(line, "ip -o -4 addr show eth0") {
			for gw, addr := range addrs {
				if strings.Contains(line, gw) {
					return addr + "\n", nil
				}
			}
		}
		return labWithConfig("stale config\n")(argv)
	}}
	a := newTestApplier(t, cmd, "flat-dnat")

	if err := a.applyProfile(context.Background(), newJournal(&bytes.Buffer{}, newFakeClock().now)); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}

	for gw, addr := range addrs {
		vip := vipsIn(t, a.current[gw])[apiVIPAddr]
		if vip == nil {
			t.Fatalf("%s was not given the API VIP: %v", gw, a.current[gw])
		}
		rules, _ := vip["rules"].([]any)
		rule, _ := rules[0].(map[string]any)
		if rule["dest_addr"] != addr {
			t.Fatalf("%s forwards its API VIP to %v, want its own management address %s",
				gw, rule["dest_addr"], addr)
		}
	}
}

// A gateway that never comes back from its config swap leaves the lab in
// a state the run cannot measure — the roll stops rather than taking the
// next gateway down on top of it.
func TestApplyProfileFailsWhenAGatewayNeverComesBack(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "{{.State.Health.Status}}") {
			return "starting\n", nil
		}
		return labWithConfig("stale config\n")(argv)
	}}
	a := newTestApplier(t, cmd, "flat-minimal")

	err := a.applyProfile(context.Background(), newJournal(&bytes.Buffer{}, newFakeClock().now))

	if err == nil {
		t.Fatal("a gateway that never came back was reported as reconfigured")
	}
	if !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("error %q does not say the gateway never returned", err)
	}
	if got := cmd.count(restartCmd); got != 1 {
		t.Fatalf("restarted %d gateways, want the roll to stop at the one that never came back", got)
	}
}
