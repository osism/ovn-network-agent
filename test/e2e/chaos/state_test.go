package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
		// cross-chassis-fip.sh: the second router and its FIP.
		"lr-nat-add lr1 dnat_and_snat 192.0.2.20 192.168.20.10",
		// Every router beside lr0 has its active chassis and a standby.
		"lrp-set-gateway-chassis lr-vlan101-public gateway-1 30",
		"lrp-set-gateway-chassis lr-vlan101-public gateway-2 20",
		"lrp-set-gateway-chassis lr-vlan102-public gateway-1 30",
		"lrp-set-gateway-chassis lr-vlan102-public gateway-2 20",
		"lrp-set-gateway-chassis lr1-public gateway-2 30",
		"lrp-set-gateway-chassis lr1-public gateway-3 20",
		// Every responder behind a probed FIP.
		"external_ids:iface-id=ls0-vm2",
		"external_ids:iface-id=vm101",
		"external_ids:iface-id=vm102",
		"external_ids:iface-id=ls1-vm3",
	} {
		if !cmd.called(want) {
			t.Fatalf("the start state did not issue %q", want)
		}
	}
}

// A router with one Gateway_Chassis row cannot fail over: a fault on that
// chassis darkens its FIPs for the whole hold, whatever the agent does.
// So every router the start state binds has two candidates, both
// gateways, at two priorities, so that OVN has one owner to elect.
func TestStartStateGivesEveryRouterTwoGatewayChassis(t *testing.T) {
	wantRows := map[string]int{
		"everything-on": 6, "vlan-no-dnat": 6, "heterogeneous": 6, "drain-everywhere": 6,
		"flat-dnat": 2, // the lr1-public pair
	}

	for _, p := range profiles() {
		t.Run(p.name, func(t *testing.T) {
			cmd := &fakeCommander{respond: healthyLabResponses}
			l := newTestLab(cmd, newFakeClock())

			if err := applyStartState(context.Background(), l, p); err != nil {
				t.Fatalf("applyStartState: %v", err)
			}

			rows := 0
			priorities := map[string]map[string]int{} // LRP -> chassis -> priority
			for _, line := range cmd.lines() {
				_, row, found := strings.Cut(line, "lrp-set-gateway-chassis ")
				if !found {
					continue
				}
				rows++
				fields := strings.Fields(row)
				if len(fields) != 3 {
					t.Fatalf("%q does not name a port, a chassis and a priority", line)
				}
				lrp, chassis := fields[0], fields[1]
				priority, err := strconv.Atoi(fields[2])
				if err != nil {
					t.Fatalf("%q carries a priority that is not a number: %v", line, err)
				}
				if !slices.Contains(gatewayNames(), chassis) {
					t.Fatalf("%s is bound to %q, which is not a gateway", lrp, chassis)
				}
				if priorities[lrp] == nil {
					priorities[lrp] = map[string]int{}
				}
				priorities[lrp][chassis] = priority
			}

			if rows != wantRows[p.name] {
				t.Fatalf("%s issued %d Gateway_Chassis rows, want %d: %v", p.name, rows, wantRows[p.name], priorities)
			}
			for lrp, byChassis := range priorities {
				if len(byChassis) != 2 {
					t.Fatalf("%s has %d candidate chassis, want an active one and a standby: %v", lrp, len(byChassis), byChassis)
				}
				distinct := map[int]bool{}
				for _, priority := range byChassis {
					distinct[priority] = true
				}
				if len(distinct) != 2 {
					t.Fatalf("%s has both candidates at one priority, so no chassis is the elected owner: %v", lrp, byChassis)
				}
			}
		})
	}
}

// A profile without the VLAN and cross-chassis layers has no router
// beside lr0, whose rows are the bootstrap's, so its start state writes
// no Gateway_Chassis row at all.
func TestStartStateWithoutLayersSetsNoGatewayChassis(t *testing.T) {
	for _, name := range []string{"flat-minimal", "pf-only"} {
		t.Run(name, func(t *testing.T) {
			cmd := &fakeCommander{respond: healthyLabResponses}
			l := newTestLab(cmd, newFakeClock())

			if err := applyStartState(context.Background(), l, testProfile(t, name)); err != nil {
				t.Fatalf("applyStartState: %v", err)
			}
			if got := cmd.count("lrp-set-gateway-chassis"); got != 0 {
				t.Fatalf("%s set %d Gateway_Chassis rows without a router to bind: %v", name, got, cmd.lines())
			}
		})
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
// service: it brings the underlay back. It rebuilds no responder and
// restarts no port-forward backend, not even on gateway-3, where
// bootstrap.sh puts vm1 for the scenarios: the start state moves every
// workload to the compute chassis.
func TestRestoreNodeRewiresTheUnderlayWithoutRebuildingWorkloads(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := restoreNode(context.Background(), l, defaultTestProfile(t), "gateway-3"); err != nil {
		t.Fatalf("restoreNode: %v", err)
	}

	if !cmd.called("containerlab tools veth create") {
		t.Fatalf("restore did not rewire the underlay: %v", cmd.lines())
	}
	for _, unwanted := range []string{"external_ids:iface-id=", pfBackendLog} {
		if cmd.called(unwanted) {
			t.Fatalf("restoring a gateway issued %q, but no workload lives on a gateway: %v", unwanted, cmd.lines())
		}
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
	if !errors.Is(err, errBoom) {
		t.Fatalf("error %q does not wrap the create's own error", err)
	}
	// Only a name collision is retried: any other failure is attempted once
	// and deletes nothing.
	if got := cmd.count("containerlab tools veth create"); got != 1 {
		t.Fatalf("veth create ran %d times after a failure that is not a collision, want once: %v",
			got, cmd.lines())
	}
	if cmd.called("ip link del") {
		t.Fatalf("deleted a link after a failure that is not a collision: %v", cmd.lines())
	}
}

// Every responder the probe set depends on must be in the table the start
// state creates on the workload host — a FIP without its responder would
// keep the start state from ever going green.
func TestEveryProbedFIPHasAResponderInTheStartState(t *testing.T) {
	lsps := map[string]bool{}
	for _, n := range responders(defaultTestProfile(t)) {
		lsps[n.lsp] = true
	}
	for _, want := range []string{"ls0-vm1", "ls0-vm2", "vm101", "vm102", "ls1-vm3"} {
		if !lsps[want] {
			t.Fatalf("responder for %s is not created by the start state", want)
		}
	}
}

// bootstrap.sh leaves vm1 on gateway-3 for the scenario tests. The start
// state has to take its OVS port away there before it binds the same
// iface-id on the compute chassis: two interfaces carrying one iface-id on
// two chassis contend for a single Port_Binding.
func TestApplyStartStateEvictsTheBootstrapWorkloadBeforeRehomingIt(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())
	p := defaultTestProfile(t)

	if err := applyStartState(context.Background(), l, p); err != nil {
		t.Fatalf("applyStartState: %v", err)
	}

	evict := cmd.indexOf("docker exec clab-ovn-e2e-gateway-3 sh -euc ovs-vsctl --if-exists del-port br-int vm1-host")
	rehome := -1
	for i, line := range cmd.lines() {
		if strings.Contains(line, "docker exec clab-ovn-e2e-compute-1") &&
			strings.Contains(line, "external_ids:iface-id=ls0-vm1") {
			rehome = i
			break
		}
	}
	if evict < 0 || rehome < 0 {
		t.Fatalf("vm1 was not evicted from gateway-3 and re-created on compute-1: %v", cmd.lines())
	}
	if evict > rehome {
		t.Fatalf("vm1 was bound on compute-1 while its port on gateway-3 still existed: %v", cmd.lines())
	}
	// Every responder the profile creates is evicted, not only vm1: an
	// interrupted scenario may have left any of them on gateway-3.
	for _, n := range responders(p) {
		if !cmd.called("clab-ovn-e2e-gateway-3 sh -euc ovs-vsctl --if-exists del-port br-int " + n.hostVeth()) {
			t.Fatalf("responder %s was not evicted from gateway-3: %v", n.name, cmd.lines())
		}
	}
	for _, line := range cmd.lines() {
		if strings.Contains(line, "clab-ovn-e2e-gateway-3") && strings.Contains(line, "external_ids:iface-id=") {
			t.Fatalf("the start state bound a workload port on gateway-3: %q", line)
		}
	}
}

// The eviction only frees the Port_Binding if it deletes the port
// bootstrap.sh created, on the node it created it on. Against any other
// name or node `ovs-vsctl --if-exists del-port` succeeds as a no-op, and
// vm1 ends up bound on two chassis while every test above stays green.
func TestBootstrapWorkloadPlacementMatchesTheEviction(t *testing.T) {
	raw, err := os.ReadFile("../bootstrap.sh")
	if err != nil {
		t.Fatalf("read bootstrap.sh: %v", err)
	}
	defaultOf := func(name string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^` + name + `="\$\{` + name + `:-([^}]+)\}"`).FindSubmatch(raw)
		if m == nil {
			t.Fatalf("bootstrap.sh declares no default for %s — the parse shape has drifted", name)
		}
		return string(m[1])
	}

	if got := defaultOf("WORKLOAD_HOST"); got != bootstrapWorkloadHost {
		t.Fatalf("bootstrap.sh puts vm1 on %s, but the start state evicts it from %s", got, bootstrapWorkloadHost)
	}
	vm1 := responders(defaultTestProfile(t))[0]
	if got, want := defaultOf("WORKLOAD_HOST_VETH"), vm1.hostVeth(); got != want {
		t.Fatalf("bootstrap.sh names vm1's OVS port %s, but the eviction deletes %s", got, want)
	}
}

// The eviction runs on every start, and from the second run on a lab
// gateway-3 holds nothing: the veth and the netns are gone, and so is the
// OVS port. That has to succeed, or no second run could start. A failing
// ovs-vsctl is another matter: the port would keep contending for the
// Port_Binding, so the eviction must fail. The script runs in a real sh
// against stubs, so this is the shell's own `-e` verdict on the guards.
func TestEvictBootstrapWorkloadsToleratesOnlyAMissingVethOrNetns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ovsExit int
		wantErr bool
	}{
		{name: "gateway-3 holds no responder", ovsExit: 0},
		{name: "the OVS port cannot be deleted", ovsExit: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubs := t.TempDir()
			for name, body := range map[string]string{
				// --if-exists turns a port that is already gone into exit 0.
				"ovs-vsctl": fmt.Sprintf("exit %d", tc.ovsExit),
				// There is no veth and no netns to delete.
				"ip": `echo "Cannot find device" >&2; exit 1`,
			} {
				if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatalf("write the %s stub: %v", name, err)
				}
			}
			cmd := &fakeCommander{respond: func(argv []string) (string, error) {
				script := exec.Command("sh", "-euc", argv[len(argv)-1])
				script.Env = []string{"PATH=" + stubs}
				out, err := script.CombinedOutput()
				return string(out), err
			}}

			err := evictBootstrapWorkloads(context.Background(), newTestLab(cmd, newFakeClock()), defaultTestProfile(t))

			if tc.wantErr {
				if err == nil {
					t.Fatalf("an OVS port that could not be deleted was reported as evicted: %v", cmd.lines())
				}
				return
			}
			if err != nil {
				t.Fatalf("evicting from a gateway that holds no responder failed: %v", err)
			}
			if got, want := cmd.count("del-port br-int"), len(responders(defaultTestProfile(t))); got != want {
				t.Fatalf("evicted %d responders, want every one of the profile's %d: %v", got, want, cmd.lines())
			}
		})
	}
}

// A failed eviction stops the start state before any responder is created
// on the compute chassis, and says which responder it could not move.
func TestEvictBootstrapWorkloadsReportsAFailedEviction(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "del-port br-int vm1-host") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := applyStartState(context.Background(), l, defaultTestProfile(t))

	if err == nil {
		t.Fatal("a start state whose eviction failed was accepted")
	}
	if !strings.Contains(err.Error(), "evict responder vm1 from gateway-3") {
		t.Fatalf("error %q does not name the responder and the host", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("error %q does not wrap the eviction's own error", err)
	}
	for _, line := range cmd.lines() {
		if strings.Contains(line, "clab-ovn-e2e-compute-1") && strings.Contains(line, "ip netns add") {
			t.Fatalf("a responder was created on compute-1 after the eviction failed: %q", line)
		}
	}
}

// A compute chassis whose br-int never shows up cannot host a responder.
// The start state gives up after the daemon-ready budget, before it has
// evicted anything from gateway-3: the scenario placement stays intact.
func TestApplyStartStateFailsWhenTheComputeChassisIsNotReady(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "clab-ovn-e2e-compute-1 sh -euc ovs-vsctl br-exists br-int") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	clock := newFakeClock()
	start := clock.now()

	err := applyStartState(context.Background(), newTestLab(cmd, clock), defaultTestProfile(t))

	if err == nil {
		t.Fatal("a start state on a compute chassis without br-int was accepted")
	}
	if !strings.Contains(err.Error(), "wait for br-int on compute-1") {
		t.Fatalf("error %q does not name the chassis that was not ready", err)
	}
	if waited := clock.now().Sub(start); waited < daemonReadyTimeout {
		t.Fatalf("gave up after %s, before the %s daemon-ready budget", waited, daemonReadyTimeout)
	}
	for _, unwanted := range []string{"del-port", "external_ids:iface-id="} {
		if cmd.called(unwanted) {
			t.Fatalf("issued %q although the compute chassis was not ready: %v", unwanted, cmd.lines())
		}
	}
}

// A responder that cannot be created names itself and the host it was
// meant for, which is the compute chassis, not a gateway.
func TestEnsureRespondersNamesTheWorkloadHostOnFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "external_ids:iface-id=ls0-vm1") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}

	err := ensureResponders(context.Background(), newTestLab(cmd, newFakeClock()), defaultTestProfile(t))

	if err == nil {
		t.Fatal("a responder that could not be created was reported as provisioned")
	}
	if !strings.Contains(err.Error(), "provision responder vm1 on compute-1") {
		t.Fatalf("error %q does not name the responder and the workload host", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("error %q does not wrap the provisioning's own error", err)
	}
}

// The namespace guard must establish that the namespace is usable, not
// merely that it is listed. A container restart destroys the namespace but
// leaves a dead anchor under /run/netns, which keeps `ip netns list`
// answering yes; provisioning then skips the re-create and fails moving the
// veth in ("Peer netns reference is invalid", EINVAL). A run on a lab whose
// workload host restarted since the previous run hits that every time, so
// the listing guard must not come back.
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
		"lr-nat-add lr1",                          // cross-chassis-fip.sh
		"external_ids:iface-id=ls0-vm2",
		"external_ids:iface-id=ls1-vm3",
		"/usr/local/bin/pf-backend",
		// No responder but vm1 is evicted or created.
		"vm2", "vm101", "vm102", "vm3",
	} {
		if cmd.called(unwanted) {
			t.Fatalf("flat-minimal layered %q, which none of its probes measure: %v", unwanted, cmd.lines())
		}
	}
	// The bootstrap responder behind the one FIP it does probe still moves
	// from gateway-3 to the workload host.
	if !cmd.called("clab-ovn-e2e-gateway-3 sh -euc ovs-vsctl --if-exists del-port br-int vm1-host") {
		t.Fatalf("the bootstrap workload was not evicted from gateway-3: %v", cmd.lines())
	}
	if !cmd.called("external_ids:iface-id=ls0-vm1") {
		t.Fatalf("the workload behind the probed FIP was not ensured: %v", cmd.lines())
	}
}

// The API VIP's backend runs in the gateway's default namespace, and only
// on the gateways whose configuration carries the VIP. It is the same
// binary as the Load_Balancer VIP's backend, so each kill pattern keys on
// its own log path, never on the binary both share.
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

	// A gateway without the API VIP has nothing to reprovision: no
	// workload lives on a gateway. gateway-3 is the one bootstrap.sh puts
	// vm1 on, under a profile that also runs the Load_Balancer backend.
	peer := &fakeCommander{respond: healthyLabResponses}
	if err := reprovisionNode(context.Background(), newTestLab(peer, newFakeClock()), p, "gateway-3"); err != nil {
		t.Fatalf("reprovision gateway-3: %v", err)
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
