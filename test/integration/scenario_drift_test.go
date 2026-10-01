//go:build integration

package integration

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover the agent's drift-recovery layer (#55,
// #279).
//
// Three mechanisms are under test:
//   - the periodic reconcile ticker (`reconcile_interval`) that re-derives
//     desired vs. actual every cycle and reinstalls anything missing,
//   - `verifyRoutes`, the post-mutation safety net that catches routes that
//     vanished between the add and the verify (historically a vtysh/FRR race),
//     and
//   - the route watch (`route_watch`), which reconciles at once when the
//     kernel reports that an owned route was deleted or replaced.
//
// Without coverage here, a refactor that drops one of the layers would not be
// caught: the unit tests in agent_test.go and route_watch_test.go exercise the
// wiring but do not observe drift on a real kernel + FRR.
//
// The ticker and verifyRoutes scenarios run with `route_watch: false`, so the
// watch cannot repair the drift before the mechanism they cover does. They use
// testenv.FastDefaults() so the periodic tick is 2s rather than the production
// 5s, keeping the suite well under the parent #42 budget. The watched
// scenarios run at `reconcile_interval: 60s`, so only the watch can repair the
// drift inside their deadline. They also hold the watch to the other half of
// its contract: a change the agent made itself is never counted as drift.

// routeDriftMetric counts the changes the route watch detected on owned routes.
const routeDriftMetric = "ovn_network_agent_route_drift_total"

// routeWatchOff pins a scenario to the repair paths that do not need the
// route watch.
var routeWatchOff = false

// requireRouteDrift fails the test unless route_drift_total{kind} reads want.
// when names the point of the scenario for the failure message.
func requireRouteDrift(t *testing.T, addr, kind string, want float64, when string) {
	t.Helper()
	v, present := testenv.ScrapeMetrics(t, addr).Value(routeDriftMetric, map[string]string{"kind": kind})
	if !present || v != want {
		t.Fatalf("%s{kind=%q} = %v (present=%v) %s, want %v", routeDriftMetric, kind, v, present, when, want)
	}
}

// requireNoRouteDrift fails the test unless both route_drift_total series read
// 0. The watch runs from the end of the startup reconcile, so at this point it
// has seen every route the agent and zebra installed or removed since.
func requireNoRouteDrift(t *testing.T, addr, when string) {
	t.Helper()
	for _, kind := range []string{"kernel", "frr"} {
		requireRouteDrift(t, addr, kind, 0, when)
	}
}

// TestScenario_DriftKernelRouteHealed (#55 scenario 1):
//
// With one FIP installed, deleting its /32 kernel route out from under the
// agent must be detected by the periodic reconcile and reinstalled within at
// most a couple of ticks. The agent re-adds the kernel route via
// `ensureRoutes` (it sees `currentKernel` no longer contains the IP, so
// `needsKernel` is true) — independent of FRR, which is unchanged.
func TestScenario_DriftKernelRouteHealed(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftk",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	cfg.RouteWatch = &routeWatchOff
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.42"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.42")

	// Sanity: agent installs the route first time round.
	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)

	// Drift: delete the kernel route directly. The agent did not initiate
	// this change, so no event-triggered reconcile fires — recovery has to
	// come from the periodic ticker.
	if out, err := exec.Command("ip", "route", "del", fip+"/32", "dev", testenv.DefaultBridgeDev).CombinedOutput(); err != nil {
		t.Fatalf("ip route del %s/32 dev %s: %v (%s)", fip, testenv.DefaultBridgeDev, err, strings.TrimSpace(string(out)))
	}

	// Within reconcile_interval (2s) + processing slack the agent must have
	// noticed the missing kernel route and reinstalled it. Allow up to 3
	// ticks before failing — drift detection is a best-effort property and
	// asserting on a single tick would be flaky.
	testenv.AssertKernelRoute(t, fip, 8*time.Second)

	// FRR was untouched, so the static route should never have left.
	testenv.AssertFRRRoute(t, fip, 1*time.Second)

	// The repair came from the ticker: with route_watch off the agent runs no
	// watcher, says so at startup, and counts no drift.
	if drift, present := testenv.ScrapeMetrics(t, addr).Value(routeDriftMetric, map[string]string{"kind": "kernel"}); !present || drift != 0 {
		t.Errorf("%s{kind=\"kernel\"} = %v (present=%v) with route_watch off, want 0", routeDriftMetric, drift, present)
	}
	if !strings.Contains(a.LogTail(100000), "route watch disabled") {
		t.Errorf("expected 'route watch disabled' in the agent log; last logs:\n%s", a.LogTail(40))
	}
}

// TestScenario_DriftFRRRouteHealed (#55 scenario 2):
//
// With one FIP installed, deleting its FRR static route out of the VRF must
// be detected by the periodic reconcile and re-added within a couple of
// ticks. The agent reissues the route via `ensureRoutes`'s `addFRR` batch
// (it sees `currentFRR` no longer contains the IP, so `needsFRR` is true).
//
// Because `addFRR` is non-empty for that cycle, this also exercises the
// `verifyRoutes` safety-net call at the end of the reconcile — but the
// primary recovery mechanism here is the periodic tick itself.
func TestScenario_DriftFRRRouteHealed(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftf",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	cfg.RouteWatch = &routeWatchOff
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.43"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.43")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)

	// Drift: rip the static route out of FRR via vtysh. The agent's
	// AddFRRRoutes uses `ip route <ip>/32 <nexthop>`; FRR's staticd
	// requires the same `<prefix> <nexthop>` arguments on the `no` form
	// (a bare `no ip route <ip>/32` is rejected as incomplete). This
	// mirrors what a buggy FRR config-replay or a careful operator would
	// do during an emergency.
	const nexthop = "169.254.0.1"
	args := []string{
		"-c", "conf t",
		"-c", "vrf " + testenv.DefaultVRFName,
		"-c", "no ip route " + fip + "/32 " + nexthop,
		"-c", "exit-vrf",
		"-c", "end",
	}
	if out, err := exec.Command("vtysh", args...).CombinedOutput(); err != nil {
		t.Fatalf("vtysh no ip route %s/32 %s: %v (%s)", fip, nexthop, err, strings.TrimSpace(string(out)))
	}

	// Allow a generous 2× tick window — vtysh is noticeably slower than
	// netlink, and we don't want flakes on a busy CI runner.
	testenv.AssertFRRRoute(t, fip, 8*time.Second)

	// Kernel was untouched, so the /32 should never have left.
	testenv.AssertKernelRoute(t, fip, 1*time.Second)
}

// TestScenario_DriftSameCycleReAdd (#55 scenario 3):
//
// Inject a kernel-route disappearance immediately after an event-triggered
// reconcile to exercise the same-cycle re-add path:
//   - install FIP A, wait for routes
//   - delete the kernel route for FIP A
//   - install FIP B (this kicks an NB-event reconcile)
//   - both FIPs must end up present in kernel and FRR
//
// During the FIP-B reconcile the agent will:
//   - see FIP B is missing in both kernel and FRR → add both
//   - see FIP A is missing in kernel (FRR is still present) → re-add via
//     `ensureRoutes` (`needsKernel=true, needsFRR=false`)
//   - because `addFRR` is non-empty (for B), call `verifyRoutes` at the end,
//     which is the actual safety-net hook this scenario covers
//
// If a future refactor drops `verifyRoutes` and reorders the
// add-then-verify into a single non-checked add, the test still passes
// because `ensureRoutes` already re-adds A. That is intentional: this test
// asserts the *outcome* (both routes present after a same-cycle drift), not
// the specific code path that got us there. The scenario fails if the agent
// loses track of A entirely while processing B.
func TestScenario_DriftSameCycleReAdd(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "drifts",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	cfg.RouteWatch = &routeWatchOff
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipA = "198.51.100.51"
		fipB = "198.51.100.52"
	)
	testenv.AddFIP(t, ctx, nb, router, fipA, "10.0.0.51")
	testenv.AssertKernelRoute(t, fipA, 10*time.Second)
	testenv.AssertFRRRoute(t, fipA, 10*time.Second)

	// Drift: delete the kernel route for FIP A. We do NOT wait for the
	// periodic ticker here — the next step injects an event-triggered
	// reconcile via NB, and we want it to observe A as missing.
	if out, err := exec.Command("ip", "route", "del", fipA+"/32", "dev", testenv.DefaultBridgeDev).CombinedOutput(); err != nil {
		t.Fatalf("ip route del %s/32 dev %s: %v (%s)", fipA, testenv.DefaultBridgeDev, err, strings.TrimSpace(string(out)))
	}

	// Trigger an NB event so the agent reconciles immediately.
	testenv.AddFIP(t, ctx, nb, router, fipB, "10.0.0.52")

	// Both FIPs must end up present. The agent has to handle "new FIP B
	// arrived" *and* "FIP A drifted" within the same reconcile cycle.
	testenv.AssertKernelRoute(t, fipA, 10*time.Second)
	testenv.AssertKernelRoute(t, fipB, 10*time.Second)
	testenv.AssertFRRRoute(t, fipA, 10*time.Second)
	testenv.AssertFRRRoute(t, fipB, 10*time.Second)
}

// TestScenario_DriftKernelRouteWatched (#279):
//
// With one FIP installed and the periodic reconcile 60s away, deleting the
// FIP's /32 kernel route must be repaired by the route watch. The kernel
// reports the deletion on netlink, the watcher finds the prefix in the
// ownership the last reconcile published, and triggers an event reconcile
// after its 100ms debounce.
//
// The design target is under 1s. The assertion allows 2s because the agent
// binary is race-instrumented.
func TestScenario_DriftKernelRouteWatched(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftkw",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "60s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.44"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.44")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)

	periodic := map[string]string{"trigger": "periodic"}
	before, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	requireNoRouteDrift(t, addr, "before the fault: the agent counted its own install")

	if out, err := exec.Command("ip", "route", "del", fip+"/32", "dev", testenv.DefaultBridgeDev).CombinedOutput(); err != nil {
		t.Fatalf("ip route del %s/32 dev %s: %v (%s)", fip, testenv.DefaultBridgeDev, err, strings.TrimSpace(string(out)))
	}

	testenv.AssertKernelRoute(t, fip, 2*time.Second)
	testenv.AssertMetricEventually(t, addr, routeDriftMetric, map[string]string{"kind": "kernel"},
		func(v float64, present bool) bool { return present && v >= 1 },
		2*time.Second)
	requireRouteDrift(t, addr, "frr", 0, "after the kernel route was repaired: the FRR static was never touched")

	// No tick ran across the repair, so it was not the ticker that put the
	// route back.
	after, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	if after != before {
		t.Errorf("reconcile_total{trigger=\"periodic\"} went from %v to %v across the repair, want it unchanged", before, after)
	}
}

// TestScenario_DriftFRRRouteWatched (#279):
//
// The FRR counterpart of TestScenario_DriftKernelRouteWatched. Removing the
// FIP's static from staticd makes zebra delete the /32 from the VRF's kernel
// table. That deletion is what the watcher sees: a route in the VRF table that
// does not carry the agent's protocol, for an IP whose static the agent owns.
//
// This scenario is also the check of an assumption the agent rests on: that
// zebra's withdrawal arrives as an RTM_DELROUTE naming the VRF's table.
//
// The assertion allows 3s because the agent binary is race-instrumented and
// AssertFRRRoute forks vtysh every 200ms.
func TestScenario_DriftFRRRouteWatched(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftfw",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "60s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.45"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.45")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)

	periodic := map[string]string{"trigger": "periodic"}
	before, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	requireNoRouteDrift(t, addr, "before the fault: the agent counted its own install")

	// The same deletion as in TestScenario_DriftFRRRouteHealed.
	const nexthop = "169.254.0.1"
	args := []string{
		"-c", "conf t",
		"-c", "vrf " + testenv.DefaultVRFName,
		"-c", "no ip route " + fip + "/32 " + nexthop,
		"-c", "exit-vrf",
		"-c", "end",
	}
	repairDeadline := time.Now().Add(3 * time.Second)
	if out, err := exec.Command("vtysh", args...).CombinedOutput(); err != nil {
		t.Fatalf("vtysh no ip route %s/32 %s: %v (%s)", fip, nexthop, err, strings.TrimSpace(string(out)))
	}

	// The drift counter is checked first. vtysh returns before zebra has
	// withdrawn the route, so a listing taken right away could still show the
	// old static. Once the watcher has counted the kernel deletion, a static
	// in the listing is the one the repair put back.
	testenv.AssertMetricEventually(t, addr, routeDriftMetric, map[string]string{"kind": "frr"},
		func(v float64, present bool) bool { return present && v >= 1 },
		2*time.Second)
	testenv.AssertFRRRoute(t, fip, time.Until(repairDeadline))
	requireRouteDrift(t, addr, "kernel", 0, "after the FRR static was repaired: the kernel route was never touched")

	after, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	if after != before {
		t.Errorf("reconcile_total{trigger=\"periodic\"} went from %v to %v across the repair, want it unchanged", before, after)
	}
}

// TestScenario_DriftOwnRemovalNotCounted (#279):
//
// The other half of the watch's contract. Removing a FIP makes the agent
// delete its kernel /32 and withdraw the FRR static, and zebra then deletes the
// /32 from the VRF's table. The kernel reports both deletions to the watcher.
// Neither is drift: the reconcile published an ownership without the FIP
// before it removed anything.
//
// A removal counted as drift would cost a full extra reconcile and move a
// counter operators alert on.
func TestScenario_DriftOwnRemovalNotCounted(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftown",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "60s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.46"
	natUUID := testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.46")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)
	// Zebra has installed the static, so its withdrawal below is a deletion
	// the kernel reports.
	vrfTable := strconv.Itoa(testenv.VRFTableID(t, testenv.DefaultVRFName))
	testenv.AssertRouteInTable(t, vrfTable, fip, testenv.VethProviderName, 5*time.Second)
	requireNoRouteDrift(t, addr, "before the removal: the agent counted its own install")

	testenv.RemoveFIP(t, ctx, nb, router, natUUID)

	testenv.AssertNoKernelRoute(t, fip, 10*time.Second)
	testenv.AssertNoFRRRoute(t, fip, 10*time.Second)
	testenv.AssertNoRouteInTable(t, vrfTable, fip, "", 5*time.Second)
	requireNoRouteDrift(t, addr, "after the agent removed the FIP's routes: it counted its own removal")
}

// TestScenario_DriftKernelRouteReplaced (#279):
//
// A foreign route that takes the place of the agent's /32 (same prefix, device
// and metric, another protocol) never deletes anything: the kernel reports one
// RTM_NEWROUTE that carries NLM_F_REPLACE. ListKernelRoutes filters on the
// agent's protocol, so the next reconcile sees the route as missing and puts
// its own back.
//
// This scenario is the check of an assumption the watcher rests on: that the
// replace flag reaches a route subscriber.
func TestScenario_DriftKernelRouteReplaced(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftkr",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "60s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.47"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.47")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)

	periodic := map[string]string{"trigger": "periodic"}
	before, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	requireNoRouteDrift(t, addr, "before the fault: the agent counted its own install")

	if out, err := exec.Command("ip", "route", "replace", fip+"/32", "dev", testenv.DefaultBridgeDev, "proto", "static").CombinedOutput(); err != nil {
		t.Fatalf("ip route replace %s/32 dev %s proto static: %v (%s)", fip, testenv.DefaultBridgeDev, err, strings.TrimSpace(string(out)))
	}

	testenv.AssertMetricEventually(t, addr, routeDriftMetric, map[string]string{"kind": "kernel"},
		func(v float64, present bool) bool { return present && v >= 1 },
		2*time.Second)
	// The /32 never left the bridge, so its presence proves nothing. The
	// repair is the route carrying the agent's protocol again.
	agentProto := strconv.Itoa(testenv.VethLeakRouteProtocol)
	testenv.Eventually(t, func() bool {
		out, err := exec.Command("ip", "-4", "route", "show", fip+"/32", "dev", testenv.DefaultBridgeDev, "proto", agentProto).CombinedOutput()
		return err == nil && strings.Contains(string(out), fip)
	}, 2*time.Second, 100*time.Millisecond, "the /32 for "+fip+" on "+testenv.DefaultBridgeDev+" does not carry the agent's protocol again")
	requireRouteDrift(t, addr, "frr", 0, "after the kernel route was repaired: the FRR static was never touched")

	after, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	if after != before {
		t.Errorf("reconcile_total{trigger=\"periodic\"} went from %v to %v across the repair, want it unchanged", before, after)
	}
}

// TestScenario_DriftFRRRouteReplaced (#279):
//
// The FRR counterpart of TestScenario_DriftKernelRouteReplaced. A foreign
// route replaces the /32 zebra installed for the FIP's static in the VRF's
// table, with a next hop other than the agent's veth next hop. The watcher
// counts the replace as `frr` drift.
//
// A replace needs the metric of the route it replaces, which is read from
// zebra's route. With any other metric the kernel adds a second route instead.
//
// Only the counter is asserted. The static is still configured, zebra merely
// stops preferring it, so ListFRRRoutes reports it present and the reconcile
// has nothing to re-add. The foreign route stays until it is deleted, which
// the scenario does at its end.
func TestScenario_DriftFRRRouteReplaced(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftfr",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "60s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.48"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.48")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 10*time.Second)
	vrfTable := strconv.Itoa(testenv.VRFTableID(t, testenv.DefaultVRFName))
	testenv.AssertRouteInTable(t, vrfTable, fip, testenv.VethProviderName, 5*time.Second)

	out, err := exec.Command("ip", "-j", "-4", "route", "show", "table", vrfTable, fip+"/32").CombinedOutput()
	if err != nil {
		t.Fatalf("ip -j route show table %s %s/32: %v (%s)", vrfTable, fip, err, strings.TrimSpace(string(out)))
	}
	var installed []struct {
		Metric int `json:"metric"`
	}
	if err := json.Unmarshal(out, &installed); err != nil || len(installed) != 1 {
		t.Fatalf("want exactly one route for %s/32 in table %s, got %q (parse error: %v)", fip, vrfTable, strings.TrimSpace(string(out)), err)
	}

	requireNoRouteDrift(t, addr, "before the fault: the agent counted its own install")

	// An on-link next hop that is neither the agent's veth next hop nor an
	// address of this host.
	foreign := []string{
		fip + "/32", "via", "169.254.0.9", "dev", testenv.VethProviderName, "onlink",
		"table", vrfTable, "metric", strconv.Itoa(installed[0].Metric),
	}
	t.Cleanup(func() {
		// Best effort: the route is already gone when the replace failed.
		_ = exec.Command("ip", append([]string{"route", "del"}, foreign...)...).Run()
	})
	if out, err := exec.Command("ip", append([]string{"route", "replace"}, foreign...)...).CombinedOutput(); err != nil {
		t.Fatalf("ip route replace %s: %v (%s)", strings.Join(foreign, " "), err, strings.TrimSpace(string(out)))
	}

	testenv.AssertMetricEventually(t, addr, routeDriftMetric, map[string]string{"kind": "frr"},
		func(v float64, present bool) bool { return present && v >= 1 },
		2*time.Second)
	requireRouteDrift(t, addr, "kernel", 0, "after the FRR route was replaced: the kernel route was never touched")
}

// TestScenario_DriftLeakRouteWatched (#279):
//
// With a locally active router and the veth leak on, the agent owns one more
// kind of route: the per-network route in the VRF's table, tagged with its own
// protocol. Deleting it is `kernel` drift, and the reconcile the watcher
// triggers puts it back through ReconcileVethLeakNetworks.
//
// The assertions allow 3s because the leak networks are reconciled after the
// announce, late in the cycle of a race-instrumented binary.
func TestScenario_DriftLeakRouteWatched(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const network = "198.51.100.0/24"
	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "driftlw",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "60s"
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertVethRouteInVRF(t, network, 15*time.Second)

	periodic := map[string]string{"trigger": "periodic"}
	before, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	requireNoRouteDrift(t, addr, "before the fault: the agent counted its own install")

	vrfTable := strconv.Itoa(testenv.VRFTableID(t, testenv.DefaultVRFName))
	if out, err := exec.Command("ip", "route", "del", network, "table", vrfTable).CombinedOutput(); err != nil {
		t.Fatalf("ip route del %s table %s: %v (%s)", network, vrfTable, err, strings.TrimSpace(string(out)))
	}

	testenv.AssertMetricEventually(t, addr, routeDriftMetric, map[string]string{"kind": "kernel"},
		func(v float64, present bool) bool { return present && v >= 1 },
		3*time.Second)
	testenv.AssertVethRouteInVRF(t, network, 3*time.Second)
	requireRouteDrift(t, addr, "frr", 0, "after the leak route was repaired: no FRR static was touched")

	after, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	if after != before {
		t.Errorf("reconcile_total{trigger=\"periodic\"} went from %v to %v across the repair, want it unchanged", before, after)
	}
}
