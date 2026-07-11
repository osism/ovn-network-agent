//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// NetworkFilters / manual network_cidrs scenarios for issue #57.
//
// The agent decides which external IPs are "managed" two ways:
//   - auto-discovery from Logical_Router_Port.networks (default; covered by #42)
//   - manual override via the `network_cidr` config key, which takes precedence
//     over discovery (config.go's effectiveNetworkFilters)
//
// The manual override is the operator's only knob for opting some networks out
// of BGP announcement. An external IP that falls outside the configured CIDRs
// MUST NOT install kernel/FRR routes — otherwise the agent silently announces
// networks the operator explicitly excluded.
//
// Path 2 was previously untested at the integration level; these scenarios pin
// it down so a precedence flip or a typo in CIDR parsing is caught.
//
// Scenario 4 from the issue ("invalid CIDR fails fast") is unit-covered:
// TestValidateConfig in config_test.go has "invalid CIDR" and "one valid
// one invalid CIDR" subtests asserting validateConfig rejects malformed
// entries before the agent reaches Run().

// TestScenario_NetworkCIDRsManualOverridesDiscovery (#57 scenario 1):
//
// With a router whose LRP carries 198.51.100.11/24 but the agent configured
// with network_cidr=["10.99.0.0/16"], the manual filter takes precedence over
// auto-discovery. A NAT for 198.51.100.42 (inside the discovered network but
// OUTSIDE the manual filter) must NOT install routes — that is the contract
// operators rely on to keep selected networks off the BGP fabric. A NAT for
// 10.99.0.42 (inside the manual filter, outside the discovered network) must
// install kernel + FRR routes.
func TestScenario_NetworkCIDRsManualOverridesDiscovery(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "ncidr1",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.NetworkCIDRs = []string{"10.99.0.0/16"}
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipInScope    = "10.99.0.42"
		fipOutOfScope = "198.51.100.42"
	)
	testenv.AddFIP(t, ctx, nb, router, fipInScope, "10.0.0.42")
	testenv.AddFIP(t, ctx, nb, router, fipOutOfScope, "10.0.0.43")

	// The in-scope FIP is the contract: routes must appear within a normal
	// reconcile window.
	testenv.AssertKernelRoute(t, fipInScope, 10*time.Second)
	testenv.AssertFRRRoute(t, fipInScope, 10*time.Second)

	// The out-of-scope FIP is the *whole point* of the manual filter — it
	// must NOT be announced. We assert the negative after the in-scope FIP
	// has converged so the agent has demonstrably finished a reconcile that
	// could have installed it.
	testenv.AssertNoKernelRoute(t, fipOutOfScope, 1*time.Second)
	testenv.AssertNoFRRRoute(t, fipOutOfScope, 1*time.Second)
}

// TestScenario_NetworkCIDRsRestartPrunesOutOfScopeFIP (#57 scenario 2):
//
// Operators narrow the manual filter to drop a network from announcement.
// A restart with the new config must converge on the new desired set: the
// previously-installed FIP that now falls outside the filter must be gone,
// and the FIP still in scope must remain.
//
// Phase 1 runs with a wide filter covering both FIPs and default
// cleanup_on_shutdown=true, so its SIGTERM cleanup removes both routes via
// removeAllRoutes -> isManaged. Phase 2 then starts with the narrow filter
// and only the in-scope FIP gets re-installed.
//
// We assert the *outcome* (only the in-scope FIP present after Phase 2
// converges) rather than the specific cleanup path. A future refactor that
// moves the pruning from agent A's shutdown into agent B's reconcile still
// satisfies the contract.
func TestScenario_NetworkCIDRsRestartPrunesOutOfScopeFIP(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "ncidr2",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		fipKept    = "10.99.0.42"
		fipDropped = "198.51.100.42"
	)
	testenv.AddFIP(t, ctx, nb, router, fipKept, "10.0.0.44")
	testenv.AddFIP(t, ctx, nb, router, fipDropped, "10.0.0.45")

	// Phase 1: wide filter covers both. Both FIPs land in kernel + FRR.
	wideCfg := testenv.Defaults()
	wideCfg.NetworkCIDRs = []string{"10.99.0.0/16", "198.51.100.0/24"}
	a1 := readyAgent(t, wideCfg)
	testenv.AssertKernelRoute(t, fipKept, 10*time.Second)
	testenv.AssertKernelRoute(t, fipDropped, 10*time.Second)
	testenv.AssertFRRRoute(t, fipKept, 10*time.Second)
	testenv.AssertFRRRoute(t, fipDropped, 10*time.Second)

	if err := a1.Stop(20 * time.Second); err != nil {
		t.Fatalf("phase 1 agent stop: %v", err)
	}

	// Phase 2: narrow filter excludes fipDropped.
	narrowCfg := testenv.Defaults()
	narrowCfg.NetworkCIDRs = []string{"10.99.0.0/16"}
	a2 := readyAgent(t, narrowCfg)
	defer a2.Stop(15 * time.Second)

	testenv.AssertKernelRoute(t, fipKept, 10*time.Second)
	testenv.AssertFRRRoute(t, fipKept, 10*time.Second)
	testenv.AssertNoKernelRoute(t, fipDropped, 15*time.Second)
	testenv.AssertNoFRRRoute(t, fipDropped, 15*time.Second)
}

// TestScenario_NetworkCIDRsEmptyFiltersClaimAllBridge32s (#57 scenario 3,
// updated for #158):
//
// With no manual filter AND no locally-active routers, the agent still reaps
// its own leftover /32 routes on br-ex. Ownership is now scoped by the route
// protocol tag (rtproto 44) instead of the old "empty filters means manage
// everything" default: ListKernelRoutes returns only proto-44 routes, so a
// stray the agent itself planted (or left behind from a previous run) is
// observed and removed, while operator routes are ignored (covered by
// TestScenario_OperatorRouteSurvivesStandbyReconcile).
//
// We pin the contract down by planting a stray /32 on br-ex with the agent's
// own protocol number (rtproto 44) and verifying the agent reaps it.
// Path-wise: with no local routers and no port-forward VIPs, reconcile takes
// the removeAllRoutes branch (agent.go's "no locally active routers and no
// port forward VIPs"). removeAllRoutes lists the agent-owned kernel /32s —
// the proto-44 stray is one — and removes them.
func TestScenario_NetworkCIDRsEmptyFiltersClaimAllBridge32s(t *testing.T) {
	_, cancel, _, _ := startScenario(t)
	defer cancel()

	const strayIP = "198.51.100.99"
	addStrayBridgeRoute(t, strayIP)

	// Sanity: kernel actually has the route before the agent starts.
	testenv.AssertKernelRoute(t, strayIP, 1*time.Second)

	cfg := testenv.FastDefaults()
	// Intentionally no MakeLocalRouter and no NetworkCIDRs: this drives the
	// reconcile into the empty-filters branch. The state under test is
	// precisely "operator has not configured a manual filter and there are
	// no local routers right now" — a routine condition on chassis between
	// gateway moves, not an exotic edge case.
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Within a couple of reconcile ticks the agent must observe the stray
	// /32 as managed-but-not-desired and remove it. FastDefaults ticks at
	// 2s; allow ~3 ticks before failing.
	testenv.AssertNoKernelRoute(t, strayIP, 8*time.Second)
}

// addStrayBridgeRoute plants a /32 on the bridge with the agent's own protocol
// number (rtproto 44). ListKernelRoutes returns only proto-44 routes, so the
// planted route is observed as agent-owned by the reconcile loop — exactly as
// a leftover from a previous run would be — and is reaped.
func addStrayBridgeRoute(t *testing.T, ip string) {
	t.Helper()
	args := []string{"route", "add", ip + "/32", "dev", testenv.DefaultBridgeDev, "proto", "44"}
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// addOperatorBridgeRoute plants a /32 on the bridge WITHOUT the agent's
// protocol number, standing in for an operator-created debugging route. Because
// ListKernelRoutes filters on rtproto 44, this route is invisible to the agent
// and must survive reconciliation.
func addOperatorBridgeRoute(t *testing.T, ip string) {
	t.Helper()
	args := []string{"route", "add", ip + "/32", "dev", testenv.DefaultBridgeDev}
	if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
		t.Fatalf("ip %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

// delBridgeRoute removes a /32 planted on the bridge, best-effort.
func delBridgeRoute(t *testing.T, ip string) {
	t.Helper()
	_ = exec.Command("ip", "route", "del", ip+"/32", "dev", testenv.DefaultBridgeDev).Run()
}

// TestScenario_OperatorRouteSurvivesStandbyReconcile (#158 test c):
//
// An operator adds a /32 debugging route on br-ex by hand (kernel default
// protocol, not rtproto 44). On a standby node — no local routers, no manual
// filter — the agent runs the removeAllRoutes branch every reconcile. Before
// #158 the empty-filters "manage everything" default deleted this route; now
// that ListKernelRoutes is scoped to proto-44 routes, the operator route is
// invisible to the agent and must persist across several fast reconcile ticks.
func TestScenario_OperatorRouteSurvivesStandbyReconcile(t *testing.T) {
	_, cancel, _, _ := startScenario(t)
	defer cancel()

	const operatorIP = "198.51.100.77"
	addOperatorBridgeRoute(t, operatorIP)
	t.Cleanup(func() { delBridgeRoute(t, operatorIP) })

	// Sanity: the route exists before the agent starts.
	testenv.AssertKernelRoute(t, operatorIP, 1*time.Second)

	cfg := testenv.FastDefaults()
	// No MakeLocalRouter and no NetworkCIDRs: the standby/idle condition where
	// reconcile takes the removeAllRoutes branch every tick.
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// FastDefaults ticks at 2s; let ~4 ticks pass, then confirm the operator
	// route is still present — the agent never owned it, so it is never reaped.
	time.Sleep(8 * time.Second)
	testenv.AssertKernelRoute(t, operatorIP, 1*time.Second)
}
