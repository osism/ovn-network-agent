//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_DrainTimeout (#59 scenario 1):
//
// With drain_on_shutdown=true and drain_timeout=1s, the agent must give up
// the drain loop once the context deadline expires (no test-side rebind of
// the CR Port_Binding means countLocalCRPorts never reaches 0). The agent
// still proceeds to cleanup, so the kernel /32 route for the LRP must be
// gone after Stop returns.
//
// The drain code emits `"drain: timeout exceeded, proceeding with shutdown"`
// on the deadline path and returns nil; we assert that line landed in the
// captured agent log. A future change to the drain metric outcome (#39) will
// allow asserting via Prometheus instead.
func TestScenario_DrainTimeout(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "draintmo",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 5},
		},
	})
	_ = router

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	cfg.DrainTimeout = "1s"
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)

	// Wait for the LRP /32 route to land so the post-drain "route is gone"
	// assertion below is meaningful (not just "never installed").
	testenv.AssertKernelRoute(t, "198.51.100.11", 15*time.Second)

	// SIGTERM. The drain loop polls SB Port_Binding every 2s; with the
	// CR Port_Binding still bound to the local chassis, countLocalCRPorts
	// stays at 1 until the 1s drain_timeout fires. The drain code logs
	// "drain: timeout exceeded, proceeding with shutdown" and returns nil,
	// the agent then runs cleanup (CleanupOnShutdown defaults to true), and
	// Stop unblocks. 15s is plenty for a 1s drain + cleanup pass.
	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	logs := a.LogTail(100000)
	if !strings.Contains(logs, "drain: timeout exceeded") {
		t.Errorf("expected drain timeout log line not found; last logs:\n%s", a.LogTail(40))
	}

	// Cleanup must have run despite the drain timeout: the LRP /32 route
	// is gone from the bridge device.
	testenv.AssertNoKernelRoute(t, "198.51.100.11", 5*time.Second)
}

// TestScenario_DrainWithoutLocalRouters (#59 scenario 2):
//
// drain_on_shutdown=true with no local routers must be a fast no-op: the
// agent finds nothing to drain, logs the no-op, and exits cleanly within a
// couple of seconds. Pins down that DrainGateways cannot regress into a
// blocking poll when there is nothing to wait for.
func TestScenario_DrainWithoutLocalRouters(t *testing.T) {
	// No MakeLocalRouter call — NB is empty save for whatever ResetOVNState
	// leaves behind, which contains zero Gateway_Chassis entries.
	_, cancel, _, _ := startScenario(t)
	defer cancel()

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	cfg.DrainTimeout = "30s" // generous; we expect the no-op path
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)

	stopStart := time.Now()
	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	elapsed := time.Since(stopStart)
	// The drain code returns immediately when no Gateway_Chassis entries
	// match the local chassis; the remaining wall time is dominated by
	// cleanup and process teardown. 5s leaves headroom for slow CI hosts
	// while still flagging a regression into the polling loop.
	if elapsed > 5*time.Second {
		t.Errorf("Stop took %s with no local routers; expected fast no-op (<5s)", elapsed)
	}

	logs := a.LogTail(100000)
	if !strings.Contains(logs, "drain: no gateway chassis entries to drain") {
		t.Errorf("expected no-op drain log line not found; last logs:\n%s", a.LogTail(40))
	}
}

// TestScenario_DrainKeepsRoutesWhenCleanupDisabled (#59 scenario 3):
//
// drain_on_shutdown=true with cleanup_on_shutdown=false must drain (NB
// Gateway_Chassis priority → 0) but leave kernel routes and the nft
// port-forward table in place. This pins down the operator-facing contract
// that drain and cleanup are independent toggles — and is the explicit
// inverse of TestScenario_PortForwardCleanupOnSIGTERM (state remains).
//
// We mirror TestScenario_DrainOnShutdown's rebind goroutine so drain
// completes promptly (instead of timing out), which keeps the test fast.
func TestScenario_DrainKeepsRoutesWhenCleanupDisabled(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "drainkeep",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 5},
		},
	})

	cfg := testenv.Defaults()
	on := true
	off := false
	cfg.DrainOnShutdown = &on
	cfg.CleanupOnShutdown = &off
	cfg.ReconcileInterval = "2s"

	// Configure a minimal port-forward VIP so the agent installs its nft
	// table; the inverse assertion below verifies the table survives a
	// no-cleanup shutdown.
	const pfVIP = "198.51.100.88"
	cfg.PortForwardDev = testenv.PortForwardLoopbackDev
	cfg.PortForwardCTZone = intPtr(64001)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       pfVIP,
		ManageVIP: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.88",
		}},
	}}

	a := readyAgent(t, cfg)
	// If the test fatals before Stop runs, the t.Cleanup chain will
	// SIGKILL the agent — leaving nft / loopback residue. Scrub on exit
	// to keep the next scenario isolated.
	t.Cleanup(func() { testenv.ScrubPortForwardResidue(t) })

	// Wait for the LRP /32 route and the nft table to land so the
	// post-shutdown "state remains" assertions are meaningful.
	testenv.AssertKernelRoute(t, "198.51.100.11", 15*time.Second)
	testenv.AssertNftChainExists(t, testenv.DefaultNftTable, "prerouting_dnat", 15*time.Second)
	testenv.AssertVIPOnLoopback(t, pfVIP, 15*time.Second)

	// Capture the agent's managed default route so the drain helper can stamp a
	// peer readiness marker on it once the CR port has migrated (the #129
	// takeover handshake), releasing the leaving node's settle on the marker
	// instead of at drain_timeout — which is what keeps this test fast.
	managedRoute := testenv.EventuallyValue(t, func() (testenv.NBLogicalRouterStaticRoute, bool) {
		return testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0")
	}, 15*time.Second, 100*time.Millisecond, "agent must create its managed default route before drain")
	markerExtIDs := map[string]string{}
	for k, v := range managedRoute.ExternalIDs {
		markerExtIDs[k] = v
	}
	markerExtIDs["ovn-network-agent-advertised"] = "drainkeep-peer"

	// Pre-stage a peer chassis so ovn-controller does not complain when
	// the rebind goroutine moves the Port_Binding mid-drain.
	peerUUID := testenv.MakeChassis(t, ctx, sb, "drainkeep-peer")
	gcName := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)

	// When NB shows priority=0 for our local Gateway_Chassis, the helper
	// rebinds the CR Port_Binding to the peer so countLocalCRPorts → 0 and
	// stamps the readiness marker so drain completes without hitting
	// drain_timeout.
	d := testenv.StartDrainRebind(t, ctx, nb, sb, testenv.DrainRebindOpts{
		GatewayChassisName: gcName,
		CRPortUUID:         router.CRPortUUID,
		PeerChassisUUID:    peerUUID,
		MarkerRouteUUID:    managedRoute.UUID,
		MarkerExternalIDs:  markerExtIDs,
	})

	if err := a.Stop(45 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	d.Finish(t)

	// Drain happened: NB Gateway_Chassis priority is 0.
	gc, ok := testenv.FindGatewayChassis(t, ctx, nb, gcName)
	if !ok {
		t.Fatal("Gateway_Chassis disappeared after drain")
	}
	if gc.Priority != 0 {
		t.Errorf("post-drain Gateway_Chassis priority = %d, want 0", gc.Priority)
	}

	// Cleanup was skipped: the LRP /32 kernel route, the nft table, and
	// the managed VIP on loopback1 are still in place. This is the
	// inverse of TestScenario_PortForwardCleanupOnSIGTERM.
	out, err := exec.Command("ip", "-4", "route", "show", "198.51.100.11/32", "dev", testenv.DefaultBridgeDev).CombinedOutput()
	if err != nil {
		t.Fatalf("ip route show: %v (output: %q)", err, string(out))
	}
	if !strings.Contains(string(out), "198.51.100.11") {
		t.Errorf("LRP /32 route removed despite cleanup_on_shutdown=false; output: %q", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("nft", "list", "table", "ip", testenv.DefaultNftTable).CombinedOutput(); err != nil {
		t.Errorf("nft table %s removed despite cleanup_on_shutdown=false: %v (output: %q)",
			testenv.DefaultNftTable, err, strings.TrimSpace(string(out)))
	}
	// Managed VIP must still be on loopback1.
	testenv.AssertVIPOnLoopback(t, pfVIP, 2*time.Second)

	// And the agent logged the "keeping routes" branch.
	logs := a.LogTail(100000)
	if !strings.Contains(logs, "shutting down, keeping routes in place") {
		t.Errorf("expected keep-routes log line not found; last logs:\n%s", a.LogTail(40))
	}
}

// TestScenario_DrainSettleTimeoutFallback (#129):
//
// With the takeover handshake active but no peer ever writing the readiness
// marker, the leaving node's settle must fall back cleanly at drain_timeout:
// the ports migrate (the rebind goroutine unblocks the migration wait), the
// marker never names a takeover chassis, so awaitTakeoverReady logs the
// timeout fallback, proceeds to cleanup, and the LRP /32 kernel route is gone
// afterwards. This is the safe path for a takeover that never signals (e.g. a
// mixed-version fleet where the peer runs a pre-handshake agent).
func TestScenario_DrainSettleTimeoutFallback(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "drainfallback",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 5},
		},
	})

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	// Short drain_timeout so the marker-wait fallback fires quickly; no peer
	// ever stamps the readiness marker in this scenario.
	cfg.DrainTimeout = "3s"
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)

	testenv.AssertKernelRoute(t, "198.51.100.11", 15*time.Second)

	// Pre-stage a peer chassis and rebind the CR port once priority hits 0 so
	// the migration wait completes; deliberately do NOT write the readiness
	// marker, forcing the handshake to fall back at drain_timeout.
	peerUUID := testenv.MakeChassis(t, ctx, sb, "drainfallback-peer")
	gcName := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)

	// Rebind the CR port once priority hits 0 so the migration wait completes,
	// but deliberately stamp NO readiness marker (MarkerRouteUUID empty),
	// forcing the handshake to fall back at drain_timeout.
	d := testenv.StartDrainRebind(t, ctx, nb, sb, testenv.DrainRebindOpts{
		GatewayChassisName: gcName,
		CRPortUUID:         router.CRPortUUID,
		PeerChassisUUID:    peerUUID,
	})

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	d.Finish(t)

	logs := a.LogTail(100000)
	if !strings.Contains(logs, "takeover readiness marker not observed before timeout") {
		t.Errorf("expected the marker-timeout fallback log line; last logs:\n%s", a.LogTail(40))
	}

	// Cleanup still ran despite the fallback: the LRP /32 kernel route is gone.
	testenv.AssertNoKernelRoute(t, "198.51.100.11", 5*time.Second)
}

// TestScenario_DrainStuckNBWrite (#59 scenario 4, optional):
//
// Pausing ovsdb-server (NB) mid-drain to verify drain_timeout is honoured
// even when NB writes hang would catch a regression where the agent waits
// for a transaction to complete past the deadline. Implementing this
// cleanly on Ubuntu requires pausing only the NB DB without taking SB down
// — ovn-ctl on Ubuntu (`ovn-ctl pause`) toggles both. Without a way to
// pause NB in isolation the test would be flaky (paused-SB cascades into
// ovn-controller restarting our chassis row, etc.), so we skip pending a
// harness primitive that can pause NB alone.
//
// Future contributors: do not "fix" this by inserting time.Sleep or a
// stop-the-world SIGSTOP loop on ovsdb-server. The drain code is supposed
// to honour drain_timeout regardless of NB write hangs; assert via metrics
// once the drain outcome label is exposed (#39).
func TestScenario_DrainStuckNBWrite(t *testing.T) {
	t.Skip("scenario 4: no clean way to pause NB ovsdb-server alone on the Ubuntu harness")
}
