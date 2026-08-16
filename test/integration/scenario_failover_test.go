//go:build integration

package integration

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_Failover (#42 scenario 4):
//
// While the agent is running with a locally-active router, simulate ovn-northd
// rebinding the chassisredirect Port_Binding to a peer chassis (as would
// happen if the peer's Gateway_Chassis priority were raised). The agent must
// notice HasLocalRouters→false and remove the per-IP routes/flows it
// installed.
func TestScenario_Failover(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fover",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	const fip = "198.51.100.77"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.77")

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Confirm the route is in place before we trigger failover, so the
	// after-failover assertion really proves removal (not just "never
	// installed").
	testenv.AssertKernelRoute(t, fip, 15*time.Second)
	testenv.AssertFRRRoute(t, fip, 15*time.Second)

	// Insert a peer chassis and rebind the CR Port_Binding to it. This
	// matches what ovn-northd would do once the peer became higher-priority.
	peerChassis := testenv.MakeChassis(t, ctx, sb, "peer-host")
	testenv.SetCRPortChassis(t, ctx, sb, router.CRPortUUID, &peerChassis)

	testenv.AssertNoKernelRoute(t, fip, 20*time.Second)
	testenv.AssertNoFRRRoute(t, fip, 20*time.Second)
	// Hairpin flows should also be gone (no local routers => empty map).
	testenv.AssertNoOVSFlow(t, "0x998", 20*time.Second)
}

// TestScenario_FailoverTakeoverAnnounceLatencyMetric (#131):
//
// TestScenario_Failover above covers the release direction (active →
// standby); this covers the takeover direction (standby → active), which no
// other scenario exercises, and asserts the failover-latency metric fires
// end-to-end against real OVN + FRR.
//
// The chassisredirect Port_Binding starts bound to a peer chassis, so the
// agent boots as standby and installs nothing. Rebinding the CR Port_Binding
// to the local chassis — what ovn-northd does once this chassis wins the
// gateway — is routed through the agent's immediate (non-debounced) SB event
// path, which stamps the failover observation. The takeover reconcile must
// then install the kernel + FRR routes for the FIP and record exactly one
// ovn_network_agent_failover_announce_seconds sample, measured from the
// chassisredirect observation to the completed BGP announce.
func TestScenario_FailoverTakeoverAnnounceLatencyMetric(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	// Create the peer chassis first so the router's CR Port_Binding can start
	// bound to it — the agent then boots as standby (no local routers).
	peerChassis := testenv.MakeChassis(t, ctx, sb, "peer-host")
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "ftake",
		LRPNetworks: []string{"198.51.100.11/24"},
		ChassisUUID: peerChassis,
	})
	const fip = "198.51.100.66"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.66")

	cfg := testenv.Defaults()
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Standby pre-condition: with the CR port bound to the peer the agent must
	// install nothing, so the post-takeover assertion really proves a takeover
	// install (not a route that was there all along). Short timeouts suffice —
	// nothing should ever appear.
	testenv.AssertNoKernelRoute(t, fip, 5*time.Second)
	testenv.AssertNoFRRRoute(t, fip, 5*time.Second)

	// The histogram is registered from startup, so its _count series is
	// present at 0 before any sample fires. The failover-record guard skips
	// the startup trigger, so no takeover sample has been recorded yet.
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_failover_announce_seconds_count", nil,
		func(v float64, present bool) bool { return present && v == 0 },
		5*time.Second)

	// Trigger the takeover: rebind the CR Port_Binding from the peer to the
	// local chassis, as ovn-northd would once this chassis wins the gateway.
	// The agent's SB event handler routes this through the immediate
	// (non-debounced) path, which stamps the failover observation.
	localUUID := testenv.LocalChassisUUID(t, ctx, sb)
	testenv.SetCRPortChassis(t, ctx, sb, router.CRPortUUID, &localUUID)

	// The takeover reconcile must install the kernel + FRR routes for the FIP
	// — same routes as the steady active state, only the ordering differs.
	testenv.AssertKernelRoute(t, fip, 20*time.Second)
	testenv.AssertFRRRoute(t, fip, 20*time.Second)

	// ...and record at least one failover-announce latency sample, closing the
	// immediate-path stamp → takeover reconcile → announce → record chain.
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_failover_announce_seconds_count", nil,
		func(v float64, present bool) bool { return present && v >= 1 },
		20*time.Second)
}

// TestScenario_StaleChassisCleanup (#42 scenario 5):
//
// A managed NB static route tagged with the chassis name of a peer that
// disappears from SB Chassis must be cleaned up by any surviving agent after
// stale_chassis_grace_period elapses. The grace period is forced down to 2s
// for this test; jitter is bounded by maxStaleCleanupJitter (≤30s in
// production) but the test gives a generous 60s deadline to avoid flakes.
func TestScenario_StaleChassisCleanup(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	// Local router so the agent enters the productive branch every reconcile.
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "stale",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	// Insert the peer chassis and seed a managed route tagged for it. The
	// route is on a *local* router because CleanupStaleChassisManagedEntries
	// only walks routes that the local agent can prove ownership of via the
	// chassis tag, not by router locality.
	peerName := "ghost-host"
	peerUUID := testenv.MakeChassis(t, ctx, sb, peerName)
	staleRouteUUID := testenv.SeedManagedRoute(t, ctx, nb, router,
		"203.0.113.99/32", "169.254.0.1", peerName)

	// Sanity: route is present before we delete the chassis.
	if testenv.CountManagedRoutes(t, ctx, nb, peerName) != 1 {
		t.Fatalf("seeded route not present (uuid=%s)", staleRouteUUID)
	}

	cfg := testenv.Defaults()
	staleGrace := "2s"
	cfg.StaleChassisGracePeriod = staleGrace
	cfg.ReconcileInterval = "2s" // tighten so the stale check runs quickly
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// While the chassis is alive, the agent must NOT remove the route — even
	// if a reconcile cycle ran in between.
	time.Sleep(3 * time.Second)
	if got := testenv.CountManagedRoutes(t, ctx, nb, peerName); got != 1 {
		t.Fatalf("agent removed route while chassis is alive (count=%d)", got)
	}

	// Delete the chassis; after grace + jitter the route must be gone.
	// Worst-case: 2s grace + 30s jitter + reconcile interval + safety = ~60s.
	testenv.DeleteChassis(t, ctx, sb, peerUUID)
	testenv.Eventually(t, func() bool {
		return testenv.CountManagedRoutes(t, ctx, nb, peerName) == 0
	}, 60*time.Second, 500*time.Millisecond,
		"surviving agent must delete managed route tagged for missing chassis")
}

// TestScenario_MultipleStaleChassisCleanup (#64 scenario 3):
//
// TestScenario_StaleChassisCleanup covers exactly one ghost chassis. The
// cleanup path walks a map of stale chassis and applies a single jitter value
// per agent, so covering N>1 protects against hidden per-iteration state
// (e.g. metric labels, jitter reuse) that a one-chassis fixture cannot see.
//
// Seed two peer chassis (ghost-a, ghost-b) and one managed route tagged for
// each. Delete both at once. After grace + jitter + a reconcile tick, both
// routes must be gone.
func TestScenario_MultipleStaleChassisCleanup(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "multistale",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		peerAName = "ghost-a"
		peerBName = "ghost-b"
	)
	peerAUUID := testenv.MakeChassis(t, ctx, sb, peerAName)
	peerBUUID := testenv.MakeChassis(t, ctx, sb, peerBName)
	testenv.SeedManagedRoute(t, ctx, nb, router, "203.0.113.97/32", "169.254.0.1", peerAName)
	testenv.SeedManagedRoute(t, ctx, nb, router, "203.0.113.98/32", "169.254.0.1", peerBName)

	if got := testenv.CountManagedRoutes(t, ctx, nb, peerAName); got != 1 {
		t.Fatalf("seeded ghost-a route missing (count=%d)", got)
	}
	if got := testenv.CountManagedRoutes(t, ctx, nb, peerBName); got != 1 {
		t.Fatalf("seeded ghost-b route missing (count=%d)", got)
	}

	cfg := testenv.Defaults()
	cfg.StaleChassisGracePeriod = "2s"
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// While both chassis are alive, neither route may be removed.
	time.Sleep(3 * time.Second)
	if got := testenv.CountManagedRoutes(t, ctx, nb, peerAName); got != 1 {
		t.Fatalf("agent removed ghost-a route while chassis alive (count=%d)", got)
	}
	if got := testenv.CountManagedRoutes(t, ctx, nb, peerBName); got != 1 {
		t.Fatalf("agent removed ghost-b route while chassis alive (count=%d)", got)
	}

	// Delete both peer chassis simultaneously. After grace + jitter, both
	// managed routes must be cleaned up in the same agent. Same worst-case
	// budget as TestScenario_StaleChassisCleanup (2s grace + 30s jitter +
	// reconcile interval + safety = ~60s).
	testenv.DeleteChassis(t, ctx, sb, peerAUUID)
	testenv.DeleteChassis(t, ctx, sb, peerBUUID)

	testenv.AssertEventually(t,
		func() bool {
			return testenv.CountManagedRoutes(t, ctx, nb, peerAName) == 0 &&
				testenv.CountManagedRoutes(t, ctx, nb, peerBName) == 0
		},
		60*time.Second, 500*time.Millisecond,
		"surviving agent must delete both managed routes in a single cleanup sweep",
		func() string {
			return fmt.Sprintf("managed-route counts: %s=%d %s=%d",
				peerAName, testenv.CountManagedRoutes(t, ctx, nb, peerAName),
				peerBName, testenv.CountManagedRoutes(t, ctx, nb, peerBName))
		})
}

// TestScenario_OneStaleOneReturning (#64 scenario 4):
//
// With two ghost chassis tagged in NB, deleting only one of them must clean
// up only that one's route; the other route stays in place. After the
// returning chassis re-appears, the agent must NOT recreate the deleted
// chassis's route (managed-route revival is not a feature) but the
// missing-chassis tracker must clear once the chassis row returns.
func TestScenario_OneStaleOneReturning(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "mixedstale",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		peerAName = "ghost-a"
		peerBName = "ghost-b"
	)
	peerAUUID := testenv.MakeChassis(t, ctx, sb, peerAName)
	_ = testenv.MakeChassis(t, ctx, sb, peerBName) // peerBUUID intentionally unused — never deleted
	testenv.SeedManagedRoute(t, ctx, nb, router, "203.0.113.97/32", "169.254.0.1", peerAName)
	testenv.SeedManagedRoute(t, ctx, nb, router, "203.0.113.98/32", "169.254.0.1", peerBName)

	cfg := testenv.Defaults()
	cfg.StaleChassisGracePeriod = "2s"
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Delete only ghost-a; ghost-b stays alive.
	testenv.DeleteChassis(t, ctx, sb, peerAUUID)

	// Only ghost-a's route should be removed; ghost-b's must remain.
	testenv.Eventually(t, func() bool {
		return testenv.CountManagedRoutes(t, ctx, nb, peerAName) == 0
	}, 60*time.Second, 500*time.Millisecond,
		"agent must delete only the route tagged for the missing chassis")

	if got := testenv.CountManagedRoutes(t, ctx, nb, peerBName); got != 1 {
		t.Fatalf("agent removed ghost-b route while chassis alive (count=%d)", got)
	}

	// Re-create ghost-a's chassis. The agent must NOT re-add the deleted
	// route — managed-route revival is intentionally not a feature; the
	// route was an artefact of the deleted chassis's prior lifetime.
	testenv.MakeChassis(t, ctx, sb, peerAName)

	// Give the agent at least one reconcile tick + a small safety margin to
	// react to the new chassis. The contract is that the route stays absent
	// across the next reconcile cycle.
	time.Sleep(5 * time.Second)
	if got := testenv.CountManagedRoutes(t, ctx, nb, peerAName); got != 0 {
		t.Fatalf("agent revived deleted route after chassis returned (count=%d) — managed-route revival is not a feature", got)
	}
	if got := testenv.CountManagedRoutes(t, ctx, nb, peerBName); got != 1 {
		t.Fatalf("agent removed ghost-b route after ghost-a returned (count=%d)", got)
	}
}

// TestScenario_DrainOnShutdown (#42 scenario 6):
//
// SIGTERM with drain_on_shutdown=true must lower this chassis's
// Gateway_Chassis priority to 0 BEFORE kernel routes are torn down. The
// drain loop blocks until SB shows no local CR ports — a goroutine
// simulates ovn-northd by rebinding the CR Port_Binding once it observes
// priority=0 in NB, so the agent's drain unblocks promptly.
//
// We verify ordering by recording timestamps for both transitions and
// asserting priority_zero < route_removed.
func TestScenario_DrainOnShutdown(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "drain",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 5},
		},
	})

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	// A generous drain_timeout so a Stop that finishes well under it proves the
	// handshake — the readiness marker, not the deadline — ended the drain.
	cfg.DrainTimeout = "30s"
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)

	// Wait for at least the LRP-network /32 route to land before draining.
	testenv.AssertKernelRoute(t, "198.51.100.11", 15*time.Second)

	// Wait for the agent to create its managed default route, then capture its
	// external_ids so the drain helper can stamp a peer's readiness marker on
	// it mid-drain (simulating the takeover node completing its announce).
	managedRoute := testenv.EventuallyValue(t, func() (testenv.NBLogicalRouterStaticRoute, bool) {
		return testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0")
	}, 15*time.Second, 100*time.Millisecond, "agent must create its managed default route before drain")
	markerExtIDs := map[string]string{}
	for k, v := range managedRoute.ExternalIDs {
		markerExtIDs[k] = v
	}
	markerExtIDs["ovn-network-agent-advertised"] = "drain-peer"

	// Pre-stage a peer chassis so ovn-controller does not complain when we
	// rebind the Port_Binding mid-drain.
	peerUUID := testenv.MakeChassis(t, ctx, sb, "drain-peer")
	gcName := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)

	// A takeover-peer goroutine watches for priority 0, records the moment,
	// rebinds the CR Port_Binding to the peer so countLocalCRPorts returns 0,
	// then stamps the readiness marker on the managed default route so the
	// leaving node's handshake releases on the marker instead of the drain
	// timeout.
	d := testenv.StartDrainRebind(t, ctx, nb, sb, testenv.DrainRebindOpts{
		GatewayChassisName: gcName,
		CRPortUUID:         router.CRPortUUID,
		PeerChassisUUID:    peerUUID,
		MarkerRouteUUID:    managedRoute.UUID,
		MarkerExternalIDs:  markerExtIDs,
	})

	stopStart := time.Now()
	if err := a.Stop(45 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	stopElapsed := time.Since(stopStart)
	d.Finish(t)

	// The settle ended on the marker, not on a timer: Stop finished well under
	// the 30s drain_timeout, and the handshake logged the marker observation.
	if stopElapsed > 15*time.Second {
		t.Errorf("Stop took %s; expected a marker-driven drain well under the 30s drain_timeout", stopElapsed)
	}
	if logs := a.LogTail(100000); !strings.Contains(logs, "drain: takeover readiness marker observed") {
		t.Errorf("expected the readiness-marker log line (marker-driven settle); last logs:\n%s", a.LogTail(40))
	}

	// Capture the moment the kernel route finally disappeared.
	routeGoneAt := testenv.EventuallyValue(t, func() (time.Time, bool) {
		out, err := exec.Command("ip", "-4", "route", "show", "198.51.100.11/32", "dev", testenv.DefaultBridgeDev).CombinedOutput()
		if err == nil && len(out) == 0 {
			return time.Now(), true
		}
		return time.Time{}, false
	}, 30*time.Second, 50*time.Millisecond, "kernel route for LRP IP must eventually be removed")

	select {
	case prioAt := <-d.PriorityZeroAt:
		if !prioAt.Before(routeGoneAt) {
			t.Fatalf("priority=0 must be observed before route removal: prio=%s route_gone=%s",
				prioAt.Format(time.StampMilli), routeGoneAt.Format(time.StampMilli))
		}
	default:
		t.Fatal("never observed priority=0 in NB during drain — drain logic regressed")
	}

	// Final state: NB priority is 0 (drained, not yet restored — agent stopped
	// without restart).
	gc, ok := testenv.FindGatewayChassis(t, ctx, nb, gcName)
	if !ok {
		t.Fatal("Gateway_Chassis disappeared after drain")
	}
	if gc.Priority != 0 {
		t.Errorf("post-drain Gateway_Chassis priority = %d, want 0", gc.Priority)
	}
}

// TestScenario_RestoreDrainedOnStartup (#42 scenario 7):
//
// The agent starts with NB Gateway_Chassis already at priority 0 for this
// chassis (as if a previous shutdown had drained but not restored). On
// startup the agent must:
//   - restore the priority to 1 (RestoreDrainedGateways), and then
//   - boost it to ≥minActivePriority (=2) via EnsureActivePriorityLead so
//     it strictly outranks any peer that is also at priority 1 after
//     restore.
//
// The test seeds a peer entry at priority 0 too — so once the agent
// restores and boosts, the local entry sits at 2 against a peer at 0.
func TestScenario_RestoreDrainedOnStartup(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "restore",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 0},
			{ChassisName: "restore-peer", Priority: 0},
		},
	})

	gcLocal := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)
	gcPeer := "lrp-" + router.Name + "_restore-peer"

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// EnsureActivePriorityLead boosts to maxPeer+1 with a floor of
	// minActivePriority (=2). With peer at 0, expect local priority 2.
	testenv.Eventually(t, func() bool {
		gc, ok := testenv.FindGatewayChassis(t, ctx, nb, gcLocal)
		return ok && gc.Priority >= 2
	}, 20*time.Second, 200*time.Millisecond,
		"local Gateway_Chassis must be restored from 0 → ≥2 (1 by RestoreDrainedGateways, then boosted by EnsureActivePriorityLead)")

	// Peer entry must be untouched by RestoreDrainedGateways (different chassis).
	if peer, ok := testenv.FindGatewayChassis(t, ctx, nb, gcPeer); ok {
		if peer.Priority != 0 {
			t.Errorf("peer Gateway_Chassis priority changed: got %d, want 0", peer.Priority)
		}
	} else {
		t.Errorf("peer Gateway_Chassis %s disappeared", gcPeer)
	}
}

// TestScenario_RestoreDrainedOnStartup_DrainDisabled (#254):
//
// NB Gateway_Chassis is already at priority 0 for this chassis — as a
// previous drain-enabled shutdown leaves it — and the new instance runs
// with drain_on_shutdown=false. This is the sequence a drain-disabling
// restart produces (the chaos drain-toggle flip, or a config rollout that
// turns the flag off): the old instance drains on SIGTERM, the new one
// cannot know that it did. The startup restore must run regardless of the
// current drain setting.
//
// Unlike TestScenario_RestoreDrainedOnStartup, the chassisredirect port is
// bound to a peer chassis, so this node is a standby with no local routers —
// the shape the drain leaves behind. That keeps EnsureActivePriorityLead
// out of the picture (it returns early without local routers), so only the
// startup restore can lift the row off 0: with the restore gated on
// cfg.DrainOnShutdown the row stays at 0 forever and the chassis never
// rejoins the HA group.
func TestScenario_RestoreDrainedOnStartup_DrainDisabled(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	peerUUID := testenv.MakeChassis(t, ctx, sb, "restoreoff-peer")

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "restoreoff",
		LRPNetworks: []string{"198.51.100.11/24"},
		ChassisUUID: peerUUID,
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 0},
			{ChassisName: "restoreoff-peer", Priority: 0},
		},
	})

	gcLocal := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)
	gcPeer := "lrp-" + router.Name + "_restoreoff-peer"

	cfg := testenv.Defaults()
	off := false
	cfg.DrainOnShutdown = &off
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// The restore lifts the drained row to exactly 1 (standby level). No
	// boost may follow: this chassis owns no chassisredirect port, and a
	// standby re-entering above the restore level would risk reverse
	// failover.
	testenv.Eventually(t, func() bool {
		gc, ok := testenv.FindGatewayChassis(t, ctx, nb, gcLocal)
		return ok && gc.Priority == 1
	}, 20*time.Second, 200*time.Millisecond,
		"local Gateway_Chassis must be restored from 0 → 1 even with drain_on_shutdown=false (#254)")

	// The restore itself ran (not some other writer raising the priority):
	// the standby-restore log line is present.
	if logs := a.LogTail(100000); !strings.Contains(logs, "restore-drain: gateway chassis priority restored to standby") {
		t.Errorf("expected the restore-drain log line; tail:\n%s", a.LogTail(40))
	}

	// Peer entry must be untouched by RestoreDrainedGateways (different chassis).
	if peer, ok := testenv.FindGatewayChassis(t, ctx, nb, gcPeer); ok {
		if peer.Priority != 0 {
			t.Errorf("peer Gateway_Chassis priority changed: got %d, want 0", peer.Priority)
		}
	} else {
		t.Errorf("peer Gateway_Chassis %s disappeared", gcPeer)
	}
}
