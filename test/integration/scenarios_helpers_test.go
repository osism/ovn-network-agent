//go:build integration

package integration

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// readBridgeMAC returns the MAC of the test bridge interface. Used by
// scenarios that compare an OVN-side MAC binding to the host-side bridge MAC.
func readBridgeMAC(t *testing.T) string {
	t.Helper()
	link, err := net.InterfaceByName(testenv.DefaultBridgeDev)
	if err != nil {
		t.Fatalf("lookup %s: %v", testenv.DefaultBridgeDev, err)
	}
	mac := link.HardwareAddr.String()
	if mac == "" {
		t.Fatalf("bridge %s has no MAC", testenv.DefaultBridgeDev)
	}
	return mac
}

// startScenario is startScenarioWithTimeout with the default 90s context
// ceiling. Almost every reconciliation test in this suite is sub-3-minute by
// design and fits inside it; the long-outage reconnect scenario passes a
// larger ceiling explicitly.
func startScenario(t *testing.T) (context.Context, context.CancelFunc, client.Client, client.Client) {
	t.Helper()
	return startScenarioWithTimeout(t, 90*time.Second)
}

// startScenarioWithTimeout performs the boilerplate every reconciliation test
// shares, with a caller-chosen per-test context ceiling:
//
//   - testenv.Setup (host preconditions)
//   - PauseOVNNorthd so direct SB writes survive
//   - PauseOVNController so it doesn't unbind our hand-set chassisredirect
//     port (it would otherwise clear Port_Binding.chassis because there are
//     no SB Gateway_Chassis records to back the binding without northd)
//   - connect NB and SB clients
//   - reset state from any previous test
//
// Returns the per-test context, the NB client, and the SB client. The caller
// is responsible for spawning the agent and asserting the scenario.
func startScenarioWithTimeout(t *testing.T, timeout time.Duration) (context.Context, context.CancelFunc, client.Client, client.Client) {
	t.Helper()
	testenv.Setup(t)
	testenv.PauseOVNNorthd(t)
	testenv.PauseOVNController(t)
	testenv.EnsureBridgePatchPort(t)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	nb := testenv.NewNBClient(t, ctx)
	sb := testenv.NewSBClient(t, ctx)
	testenv.ResetOVNState(t, ctx, nb, sb)
	t.Cleanup(func() { testenv.ResetOVNState(t, context.Background(), nb, sb) })

	// On failure, dump enough state for postmortem diagnosis without an
	// operator having to ssh in and re-run commands. ctx may be cancelled
	// by the time cleanup runs (the scenario context has a bounded ceiling),
	// so use a fresh background context for the OVN dump.
	t.Cleanup(func() {
		if t.Failed() {
			dumpOVNState(t, context.Background(), nb, sb)
			testenv.DumpHostState(t)
		}
	})

	return ctx, cancel, nb, sb
}

// deleteRouter removes a Logical_Router by UUID. OVSDB referential integrity
// cascades to the LRP, NAT, Gateway_Chassis, and static_route children the
// router owned, which is what the agent's local-router detection observes
// when LRPs disappear from NB. Any SB chassisredirect Port_Binding /
// Datapath_Binding seeded by MakeLocalRouter is left in place — it now
// points at a non-existent LRP, so localLRPUUIDs[portUUID] in ovn.go no
// longer includes it. End-of-test ResetOVNState scrubs the SB residue.
func deleteRouter(t *testing.T, ctx context.Context, nb client.Client, routerUUID string) {
	t.Helper()
	ops, err := nb.Where(&testenv.NBLogicalRouter{UUID: routerUUID}).Delete()
	if err != nil {
		t.Fatalf("delete router op: %v", err)
	}
	testenv.Transact(t, ctx, nb, ops)
}

// readyAgent starts an agent with cfg and waits for the "agent running" log
// line. The agent is stopped at test cleanup if the test forgot to do so.
func readyAgent(t *testing.T, cfg testenv.AgentConfig) *testenv.AgentProc {
	t.Helper()
	a := testenv.RunAgent(t, cfg)
	rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.WaitReady(rctx); err != nil {
		t.Fatalf("agent did not become ready: %v", err)
	}
	return a
}

// dumpOVNState writes the current state of the relevant SB and NB tables to
// the test log. Call this when a scenario fails in confusing ways and you
// want to see what the agent's view of the world should look like.
func dumpOVNState(t *testing.T, ctx context.Context, nb, sb client.Client) {
	t.Helper()
	t.Logf("--- OVN STATE DUMP ---")
	t.Logf("LocalHostname=%s", testenv.LocalHostname(t))
	for _, ch := range testenv.MustList[testenv.SBChassis](t, ctx, sb) {
		t.Logf("SB Chassis: uuid=%s name=%s hostname=%s", ch.UUID, ch.Name, ch.Hostname)
	}
	for _, pb := range testenv.MustList[testenv.SBPortBinding](t, ctx, sb) {
		chassis := "<nil>"
		if pb.Chassis != nil {
			chassis = *pb.Chassis
		}
		t.Logf("SB PortBinding: uuid=%s logical_port=%s type=%s chassis=%s datapath=%s",
			pb.UUID, pb.LogicalPort, pb.Type, chassis, pb.Datapath)
	}
	for _, lrp := range testenv.MustList[testenv.NBLogicalRouterPort](t, ctx, nb) {
		t.Logf("NB LRP: uuid=%s name=%s mac=%s networks=%v",
			lrp.UUID, lrp.Name, lrp.MAC, lrp.Networks)
	}
	for _, lr := range testenv.MustList[testenv.NBLogicalRouter](t, ctx, nb) {
		t.Logf("NB LR: uuid=%s name=%s ports=%v nat=%v static_routes=%v",
			lr.UUID, lr.Name, lr.Ports, lr.Nat, lr.StaticRoutes)
	}
	for _, gc := range testenv.MustList[testenv.NBGatewayChassis](t, ctx, nb) {
		t.Logf("NB GC: uuid=%s name=%s chassis_name=%s priority=%d",
			gc.UUID, gc.Name, gc.ChassisName, gc.Priority)
	}
	for _, n := range testenv.MustList[testenv.NBNAT](t, ctx, nb) {
		t.Logf("NB NAT: uuid=%s type=%s external_ip=%s logical_ip=%s",
			n.UUID, n.Type, n.ExternalIP, n.LogicalIP)
	}
	t.Logf("--- END OVN STATE DUMP ---")
}
