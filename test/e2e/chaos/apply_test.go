package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// newTestApplier wires an applier against the fake commander, with the
// management addresses already discovered — every test below is about
// what it does with them. Its journal goes nowhere; a test that reads what
// was journaled points a.jrnl at a buffer of its own.
func newTestApplier(t *testing.T, cmd commander, name string) *applier {
	t.Helper()
	a := newApplier(newTestLab(cmd, newFakeClock()), testProfile(t, name), baseConfig(t))
	a.jrnl = newJournal(&bytes.Buffer{}, newFakeClock().now)
	if err := a.discover(context.Background()); err != nil {
		t.Fatalf("discover: %v", err)
	}
	return a
}

// restartCmd is the step that identifies a restart: restartGateway's
// SIGTERM to PID 1, issued once per restart and never by anything the
// applier writes (the baked config's own comments mention `docker
// restart`, so that would not do).
const restartCmd = "kill -TERM 1"

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

	if err := a.applyProfile(context.Background()); err != nil {
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

	err := a.applyProfile(context.Background())

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

	if err := a.applyProfile(context.Background()); err == nil {
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
	a.jrnl = newJournal(&buf, newFakeClock().now)

	if err := a.applyProfile(context.Background()); err != nil {
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
	// The heterogeneous profile overlays all three gateways, so all
	// three have to roll.
	cmd := &fakeCommander{respond: labWithConfig("stale config\n")}
	a := newTestApplier(t, cmd, "heterogeneous")

	if err := a.applyProfile(context.Background()); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}

	firstRestart := cmd.indexOf("docker exec clab-ovn-e2e-gateway-1 " + restartCmd)
	firstBack := cmd.indexOf("find Chassis name=gateway-1")
	secondRestart := cmd.indexOf("docker exec clab-ovn-e2e-gateway-2 " + restartCmd)
	secondBack := cmd.indexOf("find Chassis name=gateway-2")
	thirdRestart := cmd.indexOf("docker exec clab-ovn-e2e-gateway-3 " + restartCmd)
	if firstRestart < 0 || firstBack < 0 || secondRestart < 0 || secondBack < 0 || thirdRestart < 0 {
		t.Fatalf("the roll did not restart and gate all three overlaid gateways: %v", cmd.lines())
	}
	if firstBack > secondRestart {
		t.Fatalf("gateway-2 was restarted before gateway-1 was back: %v", cmd.lines())
	}
	if secondBack > thirdRestart {
		t.Fatalf("gateway-3 was restarted before gateway-2 was back: %v", cmd.lines())
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

	if err := a.applyProfile(context.Background()); err != nil {
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

	err := a.applyProfile(context.Background())

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

// The roll waits for the agent, not just for the container. A gateway
// whose entrypoint is still on its way to the exec answers "healthy" — OVS
// and ovn-controller are up by then — so a roll that stopped there would
// take the next gateway down while the previous one had no agent on the new
// configuration at all.
func TestApplyProfileWaitsForTheAgentNotJustTheContainer(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pgrep -f "+agentBinary) {
			return "", errExit(t, 1) // the entrypoint has not exec'd it yet
		}
		return labWithConfig("stale config\n")(argv)
	}}
	a := newTestApplier(t, cmd, "flat-minimal")

	err := a.applyProfile(context.Background())

	if err == nil {
		t.Fatal("a gateway whose agent never started was reported as reconfigured")
	}
	if !strings.Contains(err.Error(), "did not come back") {
		t.Fatalf("error %q does not say the gateway never returned", err)
	}
	if got := cmd.count(restartCmd); got != 1 {
		t.Fatalf("restarted %d gateways, want the roll to stop at the one whose agent never came back", got)
	}
}

// A flip is a rolling reconfiguration: the new configuration is validated
// with the agent's own checker, then swapped in, then the node is
// restarted onto it. Doing any of that in the other order would restart a
// gateway onto a config it cannot start with.
func TestFlipValidatesThenSwapsThenRestarts(t *testing.T) {
	cmd := &fakeCommander{respond: labWithConfig(string(baseConfig(t)))}
	a := newTestApplier(t, cmd, defaultProfileName)
	var buf bytes.Buffer
	a.jrnl = newJournal(&buf, newFakeClock().now)
	if err := a.applyProfile(context.Background()); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}
	before := len(cmd.lines())

	if err := a.flip(context.Background(), "gateway-2", flipIndex(t, "drain-toggle")); err != nil {
		t.Fatalf("flip: %v", err)
	}

	// The profile apply already staged and validated every gateway, so the
	// flip's own steps are the ones after it.
	steps := []string{
		"--check-config",
		"printf '%s' 'everything-on' > " + profileMarkerPath, // the file now owns the drain
		"mv " + agentConfigNextPath,
		"docker exec clab-ovn-e2e-gateway-2 " + restartCmd,
		"docker start clab-ovn-e2e-gateway-2",
	}
	flipped := cmd.lines()[before:]
	last := -1
	for _, step := range steps {
		at := -1
		for i, line := range flipped {
			if i > last && strings.Contains(line, step) {
				at = i
				break
			}
		}
		if at < 0 {
			t.Fatalf("the flip never issued %q, or issued it out of order — validate, swap, restart: %v",
				step, flipped)
		}
		last = at
	}
	if a.current["gateway-2"]["drain_on_shutdown"] != true {
		t.Fatalf("the tracker did not follow the flip: %v", a.current["gateway-2"])
	}

	ev := lastEventOf(t, buf.String(), evConfigFlip)
	if ev.Flip != "drain-toggle" || ev.From != "false" || ev.To != "true" {
		t.Fatalf("journaled %+v, want the drain flip and the values it moved between", ev)
	}
	if ev.Rejected == nil || *ev.Rejected {
		t.Fatalf("an applied flip was journaled as rejected: %+v", ev)
	}
}

// The whitelist is free to draw a configuration the agent refuses. That is
// not a failure of the run — it is the answer an operator gets from a
// rejected rollout — so the gateway keeps running what it was running, and
// the tracker goes back to it rather than drifting away from the lab.
func TestFlipRejectedByTheAgentLeavesTheGatewayUntouched(t *testing.T) {
	staged := false
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		// Only the flip's own validation fails: the profile has to land
		// first, or there would be nothing to flip.
		if staged && strings.Contains(line, "--check-config") {
			return "", errExit(t, 1)
		}
		return labWithConfig(string(baseConfig(t)))(argv)
	}}
	a := newTestApplier(t, cmd, defaultProfileName)
	var buf bytes.Buffer
	a.jrnl = newJournal(&buf, newFakeClock().now)
	if err := a.applyProfile(context.Background()); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}
	staged = true
	before := len(cmd.lines())

	if err := a.flip(context.Background(), "gateway-1", flipIndex(t, "cadence-toggle")); err != nil {
		t.Fatalf("a rejected flip failed the run: %v", err)
	}

	for _, line := range cmd.lines()[before:] {
		if strings.Contains(line, "mv "+agentConfigNextPath) || strings.Contains(line, restartCmd) {
			t.Fatalf("a rejected config was swapped in anyway: %q", line)
		}
	}
	if got := a.current["gateway-1"]["reconcile_interval"]; got != fastCadence {
		t.Fatalf("the tracker kept a config the gateway rejected: reconcile_interval = %v", got)
	}
	rejected := lastEventOf(t, buf.String(), evConfigFlip)
	if rejected.Rejected == nil || !*rejected.Rejected {
		t.Fatalf("journaled %+v, want the flip recorded as rejected", rejected)
	}
	if rejected.Detail == "" {
		t.Fatalf("the rejected flip was journaled without the agent's reason: %+v", rejected)
	}
}

func flipIndex(t *testing.T, name string) int {
	t.Helper()
	for i, f := range flips() {
		if f.name == name {
			return i
		}
	}
	t.Fatalf("no flip named %q in the whitelist", name)
	return -1
}

func lastEventOf(t *testing.T, journal, name string) event {
	t.Helper()
	var found event
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == name {
			found = ev
		}
	}
	if found.Event == "" {
		t.Fatalf("no %s event was journaled: %s", name, journal)
	}
	return found
}

// reloadingLab answers like a lab whose agent reloads on SIGHUP: every
// `kill -HUP 1` moves the reload counter named by verdict — "success",
// "error", or "" for an agent that never gets to it — and the loopback
// metrics scrape reports both counters.
func reloadingLab(live, verdict string) func(argv []string) (string, error) {
	hups := 0
	return func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "kill -HUP 1"):
			hups++
			return "", nil
		case strings.Contains(line, "/dev/tcp/127.0.0.1/9273"):
			ok, failed := 0, 0
			switch verdict {
			case "success":
				ok = hups
			case "error":
				failed = hups
			}
			return fmt.Sprintf("ovn_network_agent_config_reload_total{outcome=\"error\"} %d\n"+
				"ovn_network_agent_config_reload_total{outcome=\"success\"} %d\n", failed, ok), nil
		}
		return labWithConfig(live)(argv)
	}
}

// flipOnto applies the default profile and returns the applier ready for a
// flip, with the journal in buf and the commands issued so far counted.
func flipOnto(t *testing.T, cmd *fakeCommander) (*applier, *bytes.Buffer, int) {
	t.Helper()
	a := newTestApplier(t, cmd, defaultProfileName)
	var buf bytes.Buffer
	a.jrnl = newJournal(&buf, newFakeClock().now)
	if err := a.applyProfile(context.Background()); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}
	return a, &buf, len(cmd.lines())
}

// A flip the running agent applies in place goes on with a SIGHUP, not a
// restart: validate, read the reload counters, swap, SIGHUP, and take the
// counter that moved as the agent's verdict. The marker stays as it was —
// no container start means the environment the agent started with stands.
func TestFlipReloadsWhatTheAgentReloadsInPlace(t *testing.T) {
	cmd := &fakeCommander{respond: reloadingLab(string(baseConfig(t)), "success")}
	a, buf, before := flipOnto(t, cmd)

	if err := a.flip(context.Background(), "gateway-2", flipIndex(t, "cadence-toggle")); err != nil {
		t.Fatalf("flip: %v", err)
	}

	flipped := cmd.lines()[before:]
	last := -1
	for _, step := range []string{
		"--check-config", "/dev/tcp/127.0.0.1/9273", "mv " + agentConfigNextPath,
		"docker exec clab-ovn-e2e-gateway-2 kill -HUP 1", "/dev/tcp/127.0.0.1/9273",
	} {
		at := -1
		for i, line := range flipped {
			if i > last && strings.Contains(line, step) {
				at = i
				break
			}
		}
		if at < 0 {
			t.Fatalf("the reload never issued %q, or issued it out of order: %v", step, flipped)
		}
		last = at
	}
	for _, line := range flipped {
		if strings.Contains(line, profileMarkerPath) || strings.Contains(line, restartCmd) ||
			strings.Contains(line, "docker start") {
			t.Fatalf("a reloading flip touched the marker or restarted the node: %q", line)
		}
	}
	if got := a.current["gateway-2"]["reconcile_interval"]; got != slowCadence {
		t.Fatalf("the tracker did not follow the reload: reconcile_interval = %v", got)
	}
	if a.restartedByFlip("gateway-2") {
		t.Fatal("a reload was recorded as a restart — the restore would re-wire a live gateway")
	}
	ev := lastEventOf(t, buf.String(), evConfigFlip)
	if ev.Mode != flipModeReload || ev.Rejected == nil || *ev.Rejected {
		t.Fatalf("journaled %+v, want an applied reload", ev)
	}
}

// The agent validates the *merged* configuration a reload produces, so it
// can refuse one the file-level check passed. It then keeps running what
// it ran; the previous file goes back so the next restart does not pick up
// what the agent refused, and the flip is journaled as rejected.
func TestFlipReloadRefusedByTheAgentPutsThePreviousFileBack(t *testing.T) {
	cmd := &fakeCommander{respond: reloadingLab(string(baseConfig(t)), "error")}
	a, buf, before := flipOnto(t, cmd)

	if err := a.flip(context.Background(), "gateway-2", flipIndex(t, "cadence-toggle")); err != nil {
		t.Fatalf("a refused reload failed the run: %v", err)
	}

	hup := -1
	restored := -1
	for i, line := range cmd.lines()[before:] {
		switch {
		case strings.Contains(line, "kill -HUP 1"):
			hup = i
		case strings.Contains(line, "> "+agentConfigPath) && strings.Contains(line, "reconcile_interval: "+fastCadence):
			restored = i
		}
	}
	if hup < 0 || restored < hup {
		t.Fatalf("the previous config was not written back after the refused reload: %v", cmd.lines()[before:])
	}
	if got := a.current["gateway-2"]["reconcile_interval"]; got != fastCadence {
		t.Fatalf("the tracker kept a config the agent refused: reconcile_interval = %v", got)
	}
	ev := lastEventOf(t, buf.String(), evConfigFlip)
	if ev.Mode != flipModeReload || ev.Rejected == nil || !*ev.Rejected || ev.Detail == "" {
		t.Fatalf("journaled %+v, want the reload recorded as rejected, with a reason", ev)
	}
}

// An agent that never reports a verdict on the SIGHUP is a failed action,
// not a silent success: the file and the running agent may now disagree.
func TestFlipReloadTimesOutWhenTheAgentNeverAnswers(t *testing.T) {
	cmd := &fakeCommander{respond: reloadingLab(string(baseConfig(t)), "")}
	a, _, _ := flipOnto(t, cmd)

	err := a.flip(context.Background(), "gateway-2", flipIndex(t, "cadence-toggle"))

	if err == nil || !strings.Contains(err.Error(), "gateway-2") ||
		!strings.Contains(err.Error(), "did not report a configuration reload") {
		t.Fatalf("flip returned %v, want the reload timeout naming the gateway", err)
	}
}

// Without the counters' starting values no verdict can be read, so the
// flip stops before anything live is touched.
func TestFlipReloadNeedsItsBaselineCounters(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "/dev/tcp/127.0.0.1/9273") {
			return "", errBoom
		}
		return labWithConfig(string(baseConfig(t)))(argv)
	}}
	a, _, before := flipOnto(t, cmd)

	err := a.flip(context.Background(), "gateway-2", flipIndex(t, "cadence-toggle"))

	if err == nil || !strings.Contains(err.Error(), "read the reload counters on gateway-2") {
		t.Fatalf("flip returned %v, want the failed counter read", err)
	}
	for _, line := range cmd.lines()[before:] {
		if strings.Contains(line, "mv "+agentConfigNextPath) {
			t.Fatalf("the config was swapped although no verdict could be read: %q", line)
		}
	}
}

// The flips a running agent cannot apply in place keep the restart path:
// marker, swap, planned restart — and the restore has a node to put back.
func TestFlipsThatNeedARestartStillRestart(t *testing.T) {
	for _, name := range []string{"drain-toggle", "cidr-toggle"} {
		t.Run(name, func(t *testing.T) {
			cmd := &fakeCommander{respond: reloadingLab(string(baseConfig(t)), "success")}
			a, buf, before := flipOnto(t, cmd)

			if err := a.flip(context.Background(), "gateway-1", flipIndex(t, name)); err != nil {
				t.Fatalf("flip: %v", err)
			}

			flipped := strings.Join(cmd.lines()[before:], "\n")
			for _, step := range []string{profileMarkerPath, "mv " + agentConfigNextPath, restartCmd, "docker start"} {
				if !strings.Contains(flipped, step) {
					t.Fatalf("%s never issued %q: %s", name, step, flipped)
				}
			}
			if strings.Contains(flipped, "kill -HUP 1") {
				t.Fatalf("%s sent a SIGHUP the agent cannot apply", name)
			}
			if !a.restartedByFlip("gateway-1") {
				t.Fatalf("%s restarted the node but did not record it", name)
			}
			if ev := lastEventOf(t, buf.String(), evConfigFlip); ev.Mode != flipModeRestart {
				t.Fatalf("journaled %+v, want mode %q", ev, flipModeRestart)
			}
		})
	}
}

// The reload counters are read off the metrics scrape; a scrape without
// them — an agent that never reloaded, or an empty answer — reads as zero.
func TestParseMetricsReadsTheReloadCounters(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		ok, failed int
	}{
		{"empty body", "", 0, 0},
		{"no reload series", "ovn_network_agent_inactive_routes 0\n", 0, 0},
		{"both series", "# TYPE ovn_network_agent_config_reload_total counter\n" +
			"ovn_network_agent_config_reload_total{outcome=\"error\"} 1\n" +
			"ovn_network_agent_config_reload_total{outcome=\"success\"} 3\n", 3, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := parseMetrics(tc.body)
			if m.configReloadSuccess != tc.ok || m.configReloadError != tc.failed {
				t.Fatalf("reload counters = %d/%d, want %d/%d",
					m.configReloadSuccess, m.configReloadError, tc.ok, tc.failed)
			}
		})
	}
}
