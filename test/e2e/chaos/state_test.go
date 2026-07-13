package main

import (
	"context"
	"strings"
	"testing"
)

// The start state is the union of three scenarios' setups on top of the
// bootstrap baseline. All of it has to land, or the faults are injected
// into a lab the probes cannot measure.
func TestApplyStartStateLayersEveryScenarioSetup(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := applyStartState(context.Background(), l); err != nil {
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

	err := applyStartState(context.Background(), l)
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

	if err := restoreNode(context.Background(), l, workloadHost); err != nil {
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

func TestRestoreNodeReportsAFailedRewire(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "containerlab tools veth create") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := restoreNode(context.Background(), l, "gateway-2")
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
	for _, n := range responders() {
		lsps[n.lsp] = true
	}
	for _, want := range []string{"ls0-vm1", "ls0-vm2", "vm101", "vm102"} {
		if !lsps[want] {
			t.Fatalf("responder for %s is not rebuilt after its host is recycled", want)
		}
	}
}
