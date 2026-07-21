package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The start state is the union of three scenarios' setups on top of the
// bootstrap baseline. All of it has to land, or the faults are injected
// into a lab the probes cannot measure.
func TestApplyStartStateLayersEveryScenarioSetup(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := applyStartState(context.Background(), l, defaultTestProfile(t)); err != nil {
		t.Fatalf("applyStartState: %v", err)
	}

	for _, want := range []string{
		// hairpin.sh: the second FIP and its backend LSP.
		"lr-nat-add lr0 dnat_and_snat 192.0.2.12 192.168.10.12",
		"lsp-set-addresses ls0-vm2 02:00:00:00:0a:0b 192.168.10.12",
		// multi-vlan.sh: both provider networks.
		"set Logical_Switch_Port ln-vlan101 tag=101",
		"set Logical_Switch_Port ln-vlan102 tag=102",
		// pf-external.sh: the Load_Balancer and the two hand-plumbed routes.
		"lb-add pf-external 192.0.2.50:80 192.168.10.10:8080 tcp",
		"lr-lb-add lr0 pf-external",
		"ip route replace 192.0.2.50/32 via 100.64.1.2",
		"ip route replace 192.0.2.50/32 dev br-ex scope link",
		// Every responder behind a probed FIP.
		"external_ids:iface-id=ls0-vm2",
		"external_ids:iface-id=vm101",
		"external_ids:iface-id=vm102",
	} {
		if !cmd.called(want) {
			t.Fatalf("the start state did not issue %q", want)
		}
	}
}

// A lab that was never green cannot tell a fault apart from a
// pre-existing break, so the run is abandoned instead of reporting false
// violations for ten minutes.
func TestApplyStartStateFailsWhenTheLabNeverGoesGreen(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		// The VLAN FIP never answers — its router never came up.
		if strings.Contains(line, "ping -c 1 -W 1 198.51.100.10") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := applyStartState(context.Background(), l, defaultTestProfile(t))
	if err == nil {
		t.Fatal("a start state with a dead FIP was accepted")
	}
	if !strings.Contains(err.Error(), "not green") || !strings.Contains(err.Error(), "fip-vlan101") {
		t.Fatalf("error %q does not name the target that stayed red", err)
	}
}

// The VIP has to be pointed somewhere. An unbound cr-lr0-public means
// the lab has no master at all — worth failing loudly rather than
// leaving the VIP probe red for the whole run.
func TestPortForwardLayerFailsWhenTheGatewayPortIsUnbound(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "--columns=chassis find Port_Binding") {
			return "\n", nil
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := applyPortForwardLayer(context.Background(), l)
	if err == nil {
		t.Fatal("the VIP was plumbed even though no chassis owns the gateway port")
	}
	if !strings.Contains(err.Error(), crPort) {
		t.Fatalf("error %q does not name the unbound port", err)
	}
}

// restoreNode is the whole reason a killed node can be returned to
// service: the underlay first, then the node-local workloads.
func TestRestoreNodeRewiresBeforeReprovisioning(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := restoreNode(context.Background(), l, defaultTestProfile(t), workloadHost); err != nil {
		t.Fatalf("restoreNode: %v", err)
	}

	rewire := cmd.indexOf("containerlab tools veth create")
	responder := cmd.indexOf("external_ids:iface-id=ls0-vm1")
	if rewire < 0 || responder < 0 {
		t.Fatalf("restore did not rewire and reprovision: %v", cmd.lines())
	}
	if rewire > responder {
		t.Fatalf("the workloads were rebuilt before the underlay was back: %v", cmd.lines())
	}
}

// A container that reincarnates while restoreNode is repairing it — a failed
// first FRR start makes the gwnode entrypoint suicide as PID 1 and docker
// boots a second incarnation (#216) — wipes the netns the rewire configured.
// The restore must notice the identity change, re-run its rewire against the
// second incarnation, and only then declare the node back: eth1 and the BGP
// session asserted on the container that survived, not the one that is gone.
func TestRestoreNodeRewiresTheSecondIncarnationAfterAReincarnation(t *testing.T) {
	incarnation := 0
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "{{.State.StartedAt}}"):
			return fmt.Sprintf("2026-07-20T12:00:%02dZ\n", incarnation), nil
		case strings.Contains(line, "containerlab tools veth create"):
			// The first rewire lands on incarnation 0; as it wires the veth the
			// container reincarnates once, so the identity read after it differs.
			if incarnation == 0 {
				incarnation = 1
			}
			return "", nil
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	if err := restoreNode(context.Background(), l, defaultTestProfile(t), "gateway-1"); err != nil {
		t.Fatalf("restore did not survive the reincarnation: %v", err)
	}

	if got := cmd.count("containerlab tools veth create"); got != 2 {
		t.Fatalf("the rewire ran %d times, want it re-run once against the second incarnation: %v",
			got, cmd.lines())
	}
}

// A container that keeps dying after every rewire cannot be restored. The
// restore fails truthfully after one re-attempt, naming the reincarnation, so
// the run records an action-failed the artifacts can be read against instead
// of burning the recovery budget on a node that structurally cannot converge
// and blaming the lab with a recovery-timeout.
func TestRestoreNodeFailsNamingTheReincarnationWhenItNeverStabilizes(t *testing.T) {
	incarnation := 0
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "{{.State.StartedAt}}"):
			return fmt.Sprintf("2026-07-20T12:00:%02dZ\n", incarnation), nil
		case strings.Contains(line, "containerlab tools veth create"):
			incarnation++ // the container is reborn during every rewire
			return "", nil
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := restoreNode(context.Background(), l, defaultTestProfile(t), "gateway-1")
	if err == nil {
		t.Fatal("a container that never stopped reincarnating was reported as restored")
	}
	if !strings.Contains(err.Error(), "reincarnated") {
		t.Fatalf("error %q does not name the reincarnation", err)
	}
	// One re-attempt, no more: the rewire runs on the first incarnation and one
	// more, then the restore gives up rather than chasing an unstable container.
	if got := cmd.count("containerlab tools veth create"); got != 2 {
		t.Fatalf("the rewire ran %d times, want exactly the attempt and one re-attempt: %v",
			got, cmd.lines())
	}
}

func TestRestoreNodeReportsAFailedRewire(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "containerlab tools veth create") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := restoreNode(context.Background(), l, defaultTestProfile(t), "gateway-2")
	if err == nil {
		t.Fatal("a failed veth re-create was reported as a successful restore")
	}
	if !strings.Contains(err.Error(), "re-create underlay veth") {
		t.Fatalf("error %q does not name the step that failed", err)
	}
}

// Every responder the probe set depends on must be in the table that
// reprovisionNode rebuilds — a FIP whose responder is not restored would
// stay red after its host is recycled and fail the next recovery gate.
func TestEveryProbedFIPHasARestoredResponder(t *testing.T) {
	lsps := map[string]bool{}
	for _, n := range responders(defaultTestProfile(t)) {
		lsps[n.lsp] = true
	}
	for _, want := range []string{"ls0-vm1", "ls0-vm2", "vm101", "vm102"} {
		if !lsps[want] {
			t.Fatalf("responder for %s is not rebuilt after its host is recycled", want)
		}
	}
}

// The namespace guard must establish that the namespace is usable, not
// merely that it is listed. A container restart destroys the namespace but
// leaves a dead anchor under /run/netns, which keeps `ip netns list`
// answering yes; provisioning then skips the re-create and fails moving the
// veth in ("Peer netns reference is invalid", EINVAL). That is deterministic
// after every gateway restart — a profile apply or a lifecycle-fault
// restore — so the listing guard must not come back.
func TestResponderGuardSurvivesADeadNamespaceAnchor(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := ensureResponders(context.Background(), l, defaultTestProfile(t)); err != nil {
		t.Fatalf("ensureResponders: %v", err)
	}

	for _, n := range responders(defaultTestProfile(t)) {
		if cmd.called("ip netns list | awk '{print $1}' | grep -qx " + n.name) {
			t.Fatalf("responder %s is guarded by a listing that a dead anchor satisfies: %v",
				n.name, cmd.lines())
		}
		if !cmd.called("ip netns exec " + n.name + " true 2>/dev/null") {
			t.Fatalf("responder %s is not probed for usability before it is trusted: %v",
				n.name, cmd.lines())
		}
		// The dead anchor has to go before `ip netns add`, which would
		// otherwise fail with EEXIST and leave the namespace unusable.
		if !cmd.called("ip netns delete " + n.name + " 2>/dev/null || true; ip netns add " + n.name) {
			t.Fatalf("responder %s re-creates its namespace without clearing the dead anchor: %v",
				n.name, cmd.lines())
		}
	}
}

// A profile that puts up no VLAN networks must not layer them anyway: the
// point of flat-minimal is a lab whose agents have less to reconcile, and
// a stray VLAN network would leave residue no probe measures.
func TestApplyStartStateOnlyLayersTheProfilesOwnScenarios(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := applyStartState(context.Background(), l, testProfile(t, "flat-minimal")); err != nil {
		t.Fatalf("applyStartState: %v", err)
	}

	for _, unwanted := range []string{
		"ln-vlan101", // multi-vlan.sh
		"lr-nat-add lr0 dnat_and_snat 192.0.2.12", // hairpin.sh
		"lb-add pf-external",                      // pf-external.sh
		"external_ids:iface-id=ls0-vm2",
		"/usr/local/bin/pf-backend",
	} {
		if cmd.called(unwanted) {
			t.Fatalf("flat-minimal layered %q, which none of its probes measure: %v", unwanted, cmd.lines())
		}
	}
	// The bootstrap responder behind the one FIP it does probe is still
	// ensured — it is the restore path too.
	if !cmd.called("external_ids:iface-id=ls0-vm1") {
		t.Fatalf("the workload behind the probed FIP was not ensured: %v", cmd.lines())
	}
}

// The API VIP's backend runs in the gateway's default namespace, and only
// on the gateways whose configuration carries the VIP. It is the same
// binary as the Load_Balancer VIP's backend, so the two kill patterns must
// not overlap: resetting one on gateway-3 would otherwise take the other
// one down with it.
func TestStartStateStartsTheAPIBackendOnlyOnItsGateways(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := applyStartState(context.Background(), l, testProfile(t, "heterogeneous")); err != nil {
		t.Fatalf("applyStartState: %v", err)
	}

	for _, gw := range []string{"gateway-1", "gateway-2"} {
		if !cmd.called("exec -d clab-ovn-e2e-" + gw + " /usr/local/bin/pf-backend -addr :8080 -log " + apiBackendLog) {
			t.Fatalf("the API backend was not started on %s, whose config carries the VIP: %v", gw, cmd.lines())
		}
	}
	if cmd.called("exec -d clab-ovn-e2e-gateway-3 /usr/local/bin/pf-backend") {
		t.Fatalf("the API backend was started on a gateway whose config has no VIP: %v", cmd.lines())
	}
	if cmd.count("pkill -f "+apiBackendLog) != 2 {
		t.Fatalf("the API backend was reset on more than its own gateways: %v", cmd.lines())
	}
	for _, line := range cmd.lines() {
		if strings.Contains(line, "pkill") && strings.Contains(line, "/usr/local/bin/pf-backend") {
			t.Fatalf("a kill pattern matches both responders and would take the other one down: %q", line)
		}
	}
}

// A gateway that comes back from a container lifecycle event needs the
// responder behind its API VIP back too — nothing else restarts it, and
// the VIP probe would stay red for the rest of the run.
func TestReprovisionRestartsTheAPIBackendOnItsGateways(t *testing.T) {
	p := testProfile(t, "heterogeneous")
	cmd := &fakeCommander{respond: healthyLabResponses}

	if err := reprovisionNode(context.Background(), newTestLab(cmd, newFakeClock()), p, "gateway-2"); err != nil {
		t.Fatalf("reprovision gateway-2: %v", err)
	}
	if !cmd.called("exec -d clab-ovn-e2e-gateway-2 /usr/local/bin/pf-backend") {
		t.Fatalf("the API backend was not restarted after the lifecycle event: %v", cmd.lines())
	}

	// A gateway with neither node-local workloads nor an API VIP has
	// nothing to reprovision — under this profile every gateway carries
	// something, so the empty case needs one without any DNAT at all.
	peer := &fakeCommander{respond: healthyLabResponses}
	if err := reprovisionNode(context.Background(), newTestLab(peer, newFakeClock()), testProfile(t, "vlan-no-dnat"), "gateway-1"); err != nil {
		t.Fatalf("reprovision gateway-1: %v", err)
	}
	if len(peer.lines()) != 0 {
		t.Fatalf("reprovision touched a gateway that carries nothing: %v", peer.lines())
	}
}

// A backend that never binds must fail the layering rather than leave the
// API VIP probe red — and every recovery gate behind it timing out — for
// the whole run.
func TestStartAPIBackendFailsWhenTheListenerNeverBinds(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "sport = :8080") {
			return "", errBoom
		}
		return "", nil
	}}

	err := startAPIBackend(context.Background(), newTestLab(cmd, newFakeClock()), "gateway-2")

	if err == nil {
		t.Fatal("an API backend that never bound was accepted")
	}
	if !strings.Contains(err.Error(), "did not bind") {
		t.Fatalf("error %q does not say the listener never came up", err)
	}
}

// pkill exits 1 when nothing matched, which is the normal case the first
// time a responder is started. Reading that as a failure would abort the
// run before it began.
func TestStartingAResponderWithNothingToKillIsNotAFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pkill") {
			return "", errExit(t, 1)
		}
		return healthyLabResponses(argv)
	}}

	if err := startAPIBackend(context.Background(), newTestLab(cmd, newFakeClock()), "gateway-2"); err != nil {
		t.Fatalf("a first start with no previous instance failed: %v", err)
	}
	if !cmd.called("exec -d clab-ovn-e2e-gateway-2 /usr/local/bin/pf-backend") {
		t.Fatalf("the backend was never started: %v", cmd.lines())
	}

	// A pkill that could not run at all is a different matter: the old
	// instance may still hold the port, and the new one would never bind.
	broken := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pkill") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	err := startAPIBackend(context.Background(), newTestLab(broken, newFakeClock()), "gateway-2")
	if err == nil {
		t.Fatal("a reset that could not run was reported as a clean start")
	}
	if broken.called("exec -d clab-ovn-e2e-gateway-2 /usr/local/bin/pf-backend") {
		t.Fatalf("a second backend was started alongside one that may still be running: %v", broken.lines())
	}
}
