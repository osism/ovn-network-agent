package main

import (
	"context"
	"strings"
	"testing"
)

func actionNamed(t *testing.T, name string) *action {
	t.Helper()
	for _, a := range starterActions(defaultTestProfile(t)) {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no action named %q in the registry", name)
	return nil
}

// The registry order and the weights are part of the replay contract: a
// new action is appended, never inserted, or every recorded seed replays
// a different sequence.
func TestStarterRegistryOrderIsStable(t *testing.T) {
	want := []string{"controller-restart", "gateway-kill", "agent-terminate", "gateway-restart"}

	var got []string
	for _, a := range starterActions(defaultTestProfile(t)) {
		got = append(got, a.name)
		if a.weight <= 0 {
			t.Fatalf("action %s ships with weight %d", a.name, a.weight)
		}
		if a.holdMax < a.holdMin {
			t.Fatalf("action %s has holdMax %s below holdMin %s", a.name, a.holdMax, a.holdMin)
		}
		if a.recoveryBudget <= 0 {
			t.Fatalf("action %s has no recovery budget", a.name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("registry = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry = %v, want %v", got, want)
		}
	}
}

// containerlab deploys the gateways with `restart: always`. Killing one
// without disabling the policy first means docker revives it before the
// fault is observable at all — the kill would measure nothing.
func TestGatewayKillDisablesTheRestartPolicyBeforeTheKill(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	if err := actionNamed(t, "gateway-kill").inject(context.Background(), l, "gateway-2", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	disable := cmd.indexOf("update --restart=no clab-ovn-e2e-gateway-2")
	kill := cmd.indexOf("kill -s KILL clab-ovn-e2e-gateway-2")
	if disable < 0 || kill < 0 {
		t.Fatalf("kill did not disable the restart policy and SIGKILL the node: %v", cmd.lines())
	}
	if disable > kill {
		t.Fatalf("the restart policy was disabled after the kill: %v", cmd.lines())
	}
}

// Restoring a killed node has to put back what `docker start` does not:
// the containerlab veth to `upstream`, the underlay address, the BGP
// session, and the restart policy containerlab set.
func TestGatewayKillRestoreRewiresTheUnderlay(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := actionNamed(t, "gateway-kill").restore(context.Background(), l, "gateway-2"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for _, want := range []string{
		"docker start clab-ovn-e2e-gateway-2",
		"docker update --restart=always clab-ovn-e2e-gateway-2",
		"containerlab tools veth create -a clab-ovn-e2e-gateway-2:eth1 -b clab-ovn-e2e-upstream:eth2",
		"ip addr replace 100.64.2.2/30 dev eth1",
		"ip addr replace 100.64.2.1/30 dev eth2",
		"router bgp 65000 vrf vrf-provider",
	} {
		if !cmd.called(want) {
			t.Fatalf("restore did not issue %q: %v", want, cmd.lines())
		}
	}
	start := cmd.indexOf("docker start clab-ovn-e2e-gateway-2")
	rewire := cmd.indexOf("containerlab tools veth create")
	if start > rewire {
		t.Fatalf("the veth was re-created before the container was started: %v", cmd.lines())
	}
}

// The veth survives an ovn-controller restart, so it must not be
// re-created when it is still there — `veth create` would fail on an
// existing interface.
func TestRewireSkipsTheVethWhenTheInterfaceSurvived(t *testing.T) {
	// Unlike healthyLabResponses, this lab still has its eth1.
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	if err := l.rewireUnderlay(context.Background(), "gateway-1"); err != nil {
		t.Fatalf("rewireUnderlay: %v", err)
	}

	if cmd.called("containerlab tools veth create") {
		t.Fatalf("re-created a veth that was still present: %v", cmd.lines())
	}
}

// controller-restart is the soft fault: it never touches the container,
// so the containerlab veths stay up and no re-wire is needed.
func TestControllerRestartUsesOvnCtlAndTouchesNoContainer(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())
	act := actionNamed(t, "controller-restart")

	if err := act.inject(context.Background(), l, "gateway-1", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := act.restore(context.Background(), l, "gateway-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	want := []string{
		"docker exec clab-ovn-e2e-gateway-1 /usr/share/ovn/scripts/ovn-ctl stop_controller",
		"docker exec clab-ovn-e2e-gateway-1 /usr/share/ovn/scripts/ovn-ctl start_controller",
	}
	got := cmd.lines()
	if len(got) != len(want) {
		t.Fatalf("issued %d commands, want exactly the two ovn-ctl calls: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The gwnode entrypoint execs the agent, so it is PID 1 — the SIGTERM
// goes to the container's init, and the container follows it down.
func TestAgentTerminateSignalsPID1AndWaitsForTheExit(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := actionNamed(t, "agent-terminate").inject(context.Background(), l, "gateway-3", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	if !cmd.called("docker exec clab-ovn-e2e-gateway-3 kill -TERM 1") {
		t.Fatalf("the agent was not signalled as PID 1: %v", cmd.lines())
	}
	if !cmd.called("{{.State.Running}}") {
		t.Fatalf("the runner did not wait for the container to exit: %v", cmd.lines())
	}
}

// An agent that ignores its SIGTERM leaves the node in a state the run
// cannot reason about; the engine turns that error into a violation.
func TestAgentTerminateFailsWhenTheContainerStaysUp(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "{{.State.Running}}") {
			return "true\n", nil
		}
		return "", nil
	}}
	l := newTestLab(cmd, newFakeClock())

	err := actionNamed(t, "agent-terminate").inject(context.Background(), l, "gateway-3", 0)
	if err == nil {
		t.Fatal("a container that never exited was reported as a clean termination")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("error %q does not say the container stayed up", err)
	}
}

// The restart policy is pinned to "no" for the duration of the fault, so
// docker cannot revive the node behind the runner's back. A start that
// fails must still put it back — otherwise the lab keeps a gateway docker
// will never bring up again, long after the run is over.
func TestStartAndRestoreGatewayRestoresThePolicyWhenTheStartFails(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "docker start") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := startAndRestoreGateway(context.Background(), l, defaultTestProfile(t), "gateway-2")

	if err == nil {
		t.Fatal("a container that could not be started was reported as restored")
	}
	if !cmd.called("update --restart=always clab-ovn-e2e-gateway-2") {
		t.Fatalf("the restart policy was left disabled after a failed start: %v", cmd.lines())
	}
}

// gateway-restart is the odd one out twice over: `docker restart` is not
// a kill, so the restart policy must stay untouched, and the action has
// no hold — inject and restore run back to back, so the restore re-wires
// a container docker is still bringing up.
func TestGatewayRestartRecyclesTheContainerAndRestoresTheNode(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())
	act := actionNamed(t, "gateway-restart")

	if err := act.inject(context.Background(), l, workloadHost, 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := act.restore(context.Background(), l, workloadHost); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !cmd.called("docker restart clab-ovn-e2e-gateway-3") {
		t.Fatalf("the container was not recycled: %v", cmd.lines())
	}
	if cmd.called("--restart=no") {
		t.Fatalf("`docker restart` is not a kill — the restart policy must stay as containerlab set it: %v",
			cmd.lines())
	}
	rewire := cmd.indexOf("containerlab tools veth create")
	responder := cmd.indexOf("external_ids:iface-id=ls0-vm1")
	if rewire < 0 || responder < 0 {
		t.Fatalf("restore did not re-wire the underlay and rebuild the responders: %v", cmd.lines())
	}
	if rewire > responder {
		t.Fatalf("the responders were rebuilt before the underlay came back: %v", cmd.lines())
	}
}

// A container lifecycle event on the workload host destroys its network
// namespace: every responder behind a FIP and the port-forward backend
// have to be re-created. No other gateway carries node-local workloads.
func TestReprovisionRebuildsTheWorkloadHostOnly(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := reprovisionNode(context.Background(), l, defaultTestProfile(t), workloadHost); err != nil {
		t.Fatalf("reprovision %s: %v", workloadHost, err)
	}

	for _, want := range []string{
		"external_ids:iface-id=ls0-vm1",
		"external_ids:iface-id=ls0-vm2",
		"external_ids:iface-id=vm101",
		"external_ids:iface-id=vm102",
		"/usr/local/bin/pf-backend -addr :8080 -log /tmp/pf-backend.log",
	} {
		if !cmd.called(want) {
			t.Fatalf("reprovision did not restore %q: %v", want, cmd.lines())
		}
	}

	peer := &fakeCommander{respond: healthyLabResponses}
	if err := reprovisionNode(context.Background(), newTestLab(peer, newFakeClock()),
		defaultTestProfile(t), "gateway-2"); err != nil {
		t.Fatalf("reprovision gateway-2: %v", err)
	}
	if len(peer.lines()) != 0 {
		t.Fatalf("reprovision touched a gateway with no node-local workloads: %v", peer.lines())
	}
}

// The port-forward backend is restarted, not started alongside a stale
// instance, and the run waits for it to bind before probing the VIP.
func TestStartPFBackendReplacesAnyRunningInstance(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := startPFBackend(context.Background(), l); err != nil {
		t.Fatalf("startPFBackend: %v", err)
	}

	kill := cmd.indexOf("pkill -f " + pfBackendLog)
	start := cmd.indexOf("exec -d clab-ovn-e2e-gateway-3")
	listen := cmd.indexOf("sport = :8080")
	if kill < 0 || start < 0 || listen < 0 {
		t.Fatalf("backend was not reset, started and waited for: %v", cmd.lines())
	}
	if kill > start || start > listen {
		t.Fatalf("backend steps ran out of order: %v", cmd.lines())
	}
}

// A backend that never binds must fail the layering rather than let the
// VIP probe report a false violation for the rest of the run.
func TestStartPFBackendFailsWhenTheListenerNeverBinds(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "sport = :8080") {
			return "", errBoom
		}
		return "", nil
	}}
	l := newTestLab(cmd, newFakeClock())

	err := startPFBackend(context.Background(), l)
	if err == nil {
		t.Fatal("a backend that never bound was accepted")
	}
	if !strings.Contains(err.Error(), "did not bind") {
		t.Fatalf("error %q does not say the listener never came up", err)
	}
}

// The VLAN provider networks the start state layers on must land with
// their localnet tag and their router pinned, or the FIPs behind them
// never answer and every fault reads as a violation.
func TestVLANLayerTagsTheLocalnetPortAndPinsTheRouter(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	if err := applyVLANLayer(context.Background(), l, vlanNetworks[0]); err != nil {
		t.Fatalf("applyVLANLayer: %v", err)
	}

	for _, want := range []string{
		"lsp-set-options ln-vlan101 network_name=physnet1",
		"set Logical_Switch_Port ln-vlan101 tag=101",
		"lrp-set-gateway-chassis lr-vlan101-public gateway-1 30",
		"lr-nat-add lr-vlan101 dnat_and_snat 198.51.100.10 192.168.101.10",
	} {
		if !cmd.called(want) {
			t.Fatalf("vlan layer did not issue %q: %v", want, cmd.lines())
		}
	}
}

func TestVLANLayerReportsANBFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	if err := applyVLANLayer(context.Background(), l, vlanNetworks[1]); err == nil {
		t.Fatal("a failed ovn-nbctl call was swallowed")
	}
}

// 192.0.2.11 is seeded by bootstrap.sh as a NAT row with nothing behind
// it. Probing it would be red for the whole run and every recovery gate
// would time out, so it must stay out of the probe set.
func TestProbeTargetsExcludeTheBackendlessFIP(t *testing.T) {
	for _, target := range defaultProbes {
		if strings.Contains(target.addr, "192.0.2.11") {
			t.Fatalf("probe target %q has no responder behind it", target.name)
		}
	}
	if len(defaultProbes) != 5 {
		t.Fatalf("probe set = %v, want the four FIPs plus the port-forward VIP", defaultProbes)
	}
}

// The registry order and the weights are the replay contract: a new action
// is appended, never inserted, so a recorded seed still replays the
// sequence it recorded. This asserts the full ordered set and every
// action's own invariants.
func TestActionRegistryOrderIsStable(t *testing.T) {
	actions := fullRegistry(t)

	want := []string{
		// starter faults (issue #176) and the config change (issue #177)
		"controller-restart", "gateway-kill", "agent-terminate",
		"gateway-restart", "config-flip",
		// control-plane outages (issue #178)
		"nb-pause", "sb-pause", "northd-pause", "double-failover",
		// network impairment (issue #178)
		"mgmt-loss", "mgmt-delay",
		// data-plane drift (issue #178)
		"kernel-route-drop", "frr-route-drop", "nft-flush", "ovs-flow-drop",
		// routing flaps (issue #178)
		"frr-restart", "upstream-bgp-restart",
		// OVN churn (issue #178)
		"fip-churn", "lb-vip-churn", "priority-flip", "chassis-delete",
	}
	got := actionOrder(actions)
	if len(got) != len(want) {
		t.Fatalf("registry = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry = %v, want %v", got, want)
		}
	}
	for _, a := range actions {
		if a.weight <= 0 {
			t.Fatalf("action %s ships with weight %d — the weighted pick would never draw it", a.name, a.weight)
		}
		if a.holdMax < a.holdMin {
			t.Fatalf("action %s has holdMax %s below holdMin %s", a.name, a.holdMax, a.holdMin)
		}
		if a.recoveryBudget <= 0 {
			t.Fatalf("action %s has no recovery budget", a.name)
		}
	}

	// config-flip is the one action whose behaviour depends on the drawn
	// flip, and its restart onto the new config is its own undo.
	flip := actionFromRegistry(t, actions, "config-flip")
	if !flip.usesFlip || flip.applicable == nil {
		t.Fatal("config-flip does not read the flip index the engine draws for it")
	}
	if flip.holdMin != 0 || flip.holdMax != 0 {
		t.Fatalf("config-flip holds its fault for %s–%s", flip.holdMin, flip.holdMax)
	}
}

// fullRegistry builds the registry the runner drives, with the dependencies
// the actions bind to. A nil lab is fine: the actions capture it but only
// touch it at run time, and no test here executes one.
func fullRegistry(t *testing.T) []*action {
	t.Helper()
	p := defaultTestProfile(t)
	return allActions(p, nil, newApplier(nil, p, nil), newChurner(nil))
}

func actionOrder(actions []*action) []string {
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		names = append(names, a.name)
	}
	return names
}

func actionFromRegistry(t *testing.T, actions []*action, name string) *action {
	t.Helper()
	for _, a := range actions {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no action named %q in the registry: %v", name, actionOrder(actions))
	return nil
}

// The guardrail is consulted before the fault is injected, and an applier
// that has not put a profile on the lab yet tracks no configuration — so
// it reports every flip as inapplicable rather than flipping a
// configuration it has never read. (run() applies the profile before the
// engine draws anything.)
func TestAnApplierWithoutAProfileSkipsEveryFlip(t *testing.T) {
	ap := newApplier(nil, defaultTestProfile(t), nil)

	for i := range flips() {
		if ap.applicable(context.Background(), "gateway-1", i) {
			t.Fatalf("flip %s was applicable before the profile was applied", flipName(i))
		}
	}
}
