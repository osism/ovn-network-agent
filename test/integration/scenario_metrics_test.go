//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #60 — Prometheus /metrics integration
// tests. Each scenario binds the agent's metrics endpoint to a free
// loopback port (FreeLoopbackAddr) so parallel test runs do not collide,
// then scrapes the endpoint via ScrapeMetrics / AssertMetricEventually.
//
// Counters are initialised by newMetricsRegistry() with a zero observation
// per label series, so a metric that has not yet been touched still appears
// in /metrics output with value 0 — the assertions can rely on Present=true
// for known labels from the first successful scrape.

// TestScenario_MetricsReconcileCounter (#60 scenario 1):
//
// With the metrics endpoint enabled and a tightened reconcile interval, the
// startup reconcile counter must reach ≥1 by the time the agent logs "agent
// running" and the periodic counter must reach ≥1 after at least one tick
// has fired. We verify both via scrape while the agent is up; the
// scrape-on-shutdown path is unreliable because the metrics server closes
// its listener as soon as ctx is cancelled.
func TestScenario_MetricsReconcileCounter(t *testing.T) {
	_, cancel, _, _ := startScenario(t)
	defer cancel()

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "1s" // tight, so periodic increments quickly
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_reconcile_total",
		map[string]string{"trigger": "startup"},
		func(v float64, present bool) bool { return present && v >= 1 },
		10*time.Second)

	// Wait for at least one periodic tick to fire (interval is 1s; allow
	// ample slack for slow CI hosts).
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_reconcile_total",
		map[string]string{"trigger": "periodic"},
		func(v float64, present bool) bool { return present && v >= 1 },
		10*time.Second)
}

// TestScenario_MetricsDrainOutcomeCompleted (#60 scenario 2):
//
// Mirrors TestScenario_DrainOnShutdown but adds the metrics endpoint and a
// pre-shutdown scrape that confirms drain_total{outcome="completed"} is
// registered at 0. After SIGTERM the agent records the success outcome via
// recordDrain("completed", …), but the metrics HTTP listener is closed by
// srv.Shutdown as soon as ctx is cancelled — there is no race-free window
// to scrape post-drain. We therefore assert the success path via the
// agent's "drain: complete, all gateways migrated away" log line, which is
// emitted on the same code path that bumps the counter.
//
// Pinning down the post-drain counter value end-to-end would require the
// metrics server to outlive the drain phase; that is a wiring decision in
// the agent (out of scope for this issue) and is already covered by the
// unit test in metrics_test.go.
func TestScenario_MetricsDrainOutcomeCompleted(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "metdrain",
		LRPNetworks: []string{"198.51.100.11/24"},
		GatewayChassis: []testenv.GatewayChassisEntry{
			{ChassisName: testenv.LocalHostname(t), Priority: 5},
		},
	})

	cfg := testenv.Defaults()
	on := true
	cfg.DrainOnShutdown = &on
	cfg.ReconcileInterval = "2s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)

	// Wait for the LRP /32 route to land before initiating drain. Mirrors
	// TestScenario_DrainOnShutdown.
	testenv.AssertKernelRoute(t, "198.51.100.11", 15*time.Second)

	// Pre-shutdown: the drain success counter must be registered at 0.
	// newMetricsRegistry initialises every label series, so the series
	// is present from the first scrape onwards.
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_drain_total",
		map[string]string{"outcome": "completed"},
		func(v float64, present bool) bool { return present && v == 0 },
		5*time.Second)

	// Capture the agent's managed default route so the rebind goroutine can
	// stamp the peer readiness marker on it once the CR port has migrated (the
	// #129 takeover handshake). Without it the leaving node blocks in
	// awaitTakeoverReady until drain_timeout and Stop kills the agent.
	managedRoute := testenv.EventuallyValue(t, func() (testenv.NBLogicalRouterStaticRoute, bool) {
		return testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0")
	}, 15*time.Second, 100*time.Millisecond, "agent must create its managed default route before drain")
	markerExtIDs := map[string]string{}
	for k, v := range managedRoute.ExternalIDs {
		markerExtIDs[k] = v
	}
	markerExtIDs["ovn-network-agent-advertised"] = "metdrain-peer"

	// Rebind goroutine: same shape as TestScenario_DrainOnShutdown — once
	// the local Gateway_Chassis priority is lowered to 0, simulate
	// ovn-northd by rebinding the CR Port_Binding to a peer so the drain
	// loop's countLocalCRPorts returns 0 and the success path runs.
	peerUUID := testenv.MakeChassis(t, ctx, sb, "metdrain-peer")
	gcName := "lrp-" + router.Name + "_" + testenv.LocalHostname(t)

	errCh := make(chan error, 1)
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-tick.C:
				var entries []testenv.NBGatewayChassis
				if err := nb.List(ctx, &entries); err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				for _, gc := range entries {
					if gc.Name != gcName || gc.Priority != 0 {
						continue
					}
					rebind := &testenv.SBPortBinding{UUID: router.CRPortUUID, Chassis: &peerUUID}
					ops, opErr := sb.Where(rebind).Update(rebind, &rebind.Chassis)
					if opErr != nil {
						select {
						case errCh <- opErr:
						default:
						}
						return
					}
					if _, opErr := sb.Transact(ctx, ops...); opErr != nil {
						select {
						case errCh <- opErr:
						default:
						}
						return
					}
					// The takeover node has "arrived": stamp the readiness marker on
					// the managed default route so the leaving node's handshake releases
					// on the marker instead of blocking until drain_timeout.
					mark := &testenv.NBLogicalRouterStaticRoute{UUID: managedRoute.UUID, ExternalIDs: markerExtIDs}
					mops, mErr := nb.Where(mark).Update(mark, &mark.ExternalIDs)
					if mErr != nil {
						select {
						case errCh <- mErr:
						default:
						}
						return
					}
					if _, mErr := nb.Transact(ctx, mops...); mErr != nil {
						select {
						case errCh <- mErr:
						default:
						}
					}
					return
				}
			}
		}
	}()

	if err := a.Stop(45 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	pcancel()
	select {
	case err := <-errCh:
		t.Fatalf("drain helper goroutine error: %v", err)
	default:
	}

	// Drain success path: log line is emitted on the same branch that
	// records drain_total{outcome="completed"}.
	logs := a.LogTail(100000)
	if !strings.Contains(logs, "drain: complete, all gateways migrated away") {
		t.Errorf("expected drain success log line not found; last logs:\n%s", a.LogTail(40))
	}
}

// TestScenario_MetricsStaleChassisCleanup (#60 scenario 4):
//
// Mirrors TestScenario_StaleChassisCleanup but asserts the metric side of
// the cleanup: while the peer chassis is present and tracked,
// missing_chassis returns to 0; once it is deleted and the grace period
// elapses, stale_chassis_cleanup_total{outcome="success"} bumps to ≥1.
//
// Unlike the drain scenario, this happens during normal operation (no
// shutdown), so the metrics endpoint stays reachable for the assertions.
func TestScenario_MetricsStaleChassisCleanup(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "metstale",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	peerName := "metstale-ghost"
	peerUUID := testenv.MakeChassis(t, ctx, sb, peerName)
	testenv.SeedManagedRoute(t, ctx, nb, router, "203.0.113.99/32", "169.254.0.1", peerName)

	cfg := testenv.Defaults()
	cfg.StaleChassisGracePeriod = "2s"
	cfg.ReconcileInterval = "2s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// While the peer chassis is alive the agent's missing-chassis tracker
	// remains empty after every reconcile.
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_missing_chassis", nil,
		func(v float64, present bool) bool { return present && v == 0 },
		10*time.Second)

	// Delete the peer chassis; after grace + jitter the cleanup counter
	// must bump and missing_chassis must return to 0 once the stale
	// route is purged from NB. The grace period is 2s + ≤30s jitter.
	testenv.DeleteChassis(t, ctx, sb, peerUUID)

	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_stale_chassis_cleanup_total",
		map[string]string{"outcome": "success"},
		func(v float64, present bool) bool { return present && v >= 1 },
		60*time.Second)

	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_missing_chassis", nil,
		func(v float64, present bool) bool { return present && v == 0 },
		60*time.Second)
}

// TestScenario_MetricsDesiredStateGauges (#60 scenario 6):
//
// desired_ips counts every IP that needs a kernel + FRR route: the router
// gateway address from each LRP network plus every NAT external IP
// (FIP/SNAT). With one local router (LRP 198.51.100.11/24) and one FIP
// the gauge therefore settles at 2, and local_routers at 1. Adding a
// second FIP bumps desired_ips to 3 without touching local_routers. These
// gauges are updated every reconcile, so a tight ReconcileInterval keeps
// the test well under the suite's per-scenario budget.
func TestScenario_MetricsDesiredStateGauges(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "metgauge",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipA = "198.51.100.42"
		fipB = "198.51.100.43"
	)
	testenv.AddFIP(t, ctx, nb, router, fipA, "10.0.0.42")

	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_local_routers", nil,
		func(v float64, present bool) bool { return present && v == 1 },
		15*time.Second)
	// LRP gateway IP (198.51.100.11) + fipA = 2 desired IPs.
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_desired_ips", nil,
		func(v float64, present bool) bool { return present && v == 2 },
		15*time.Second)

	// Add a second FIP — desired_ips must climb to 3 (LRP + 2 FIPs).
	testenv.AddFIP(t, ctx, nb, router, fipB, "10.0.0.43")
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_desired_ips", nil,
		func(v float64, present bool) bool { return present && v == 3 },
		15*time.Second)
	// local_routers is independent of FIP count.
	testenv.AssertMetricEventually(t, addr,
		"ovn_network_agent_local_routers", nil,
		func(v float64, present bool) bool { return present && v == 1 },
		5*time.Second)
}
