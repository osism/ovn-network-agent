//go:build integration

package integration

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_FIPAddRemove (#42 scenario 1):
//
// Inserting a NAT entry into NB causes the agent to install:
//   - kernel /32 route on br-ex
//   - FRR static route in vrf-provider
//   - OVS MAC-tweak flow on br-ex (cookie 0x999)
//
// Removing the NAT entry removes all of the above. The MAC-tweak flow stays
// while any local router is active (it is a per-bridge flow, not per-IP), so
// the FIP-removal half of the test only verifies the per-IP routes are gone.
func TestScenario_FIPAddRemove(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fipr1",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)

	// Adding a NAT entry should install routes for the external IP.
	const fipExternal = "198.51.100.42"
	natUUID := testenv.AddFIP(t, ctx, nb, router, fipExternal, "10.0.0.42")

	testenv.AssertKernelRoute(t, fipExternal, 10*time.Second)
	testenv.AssertFRRRoute(t, fipExternal, 10*time.Second)
	testenv.AssertOVSFlow(t, "0x999", 10*time.Second)

	// Removing the NAT entry should withdraw both routes.
	testenv.RemoveFIP(t, ctx, nb, router, natUUID)
	testenv.AssertNoKernelRoute(t, fipExternal, 15*time.Second)
	testenv.AssertNoFRRRoute(t, fipExternal, 15*time.Second)

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
}

// TestScenario_FIPMainTableVethLeakException exercises the policy-routing
// exception needed when the FIP /32 lives in main. The destination rule is
// one priority ahead of the source-based veth-leak rule, so cross-node FIP
// traffic for a locally-hosted FIP stays in main instead of looping through
// veth-default. It must also self-heal and be removed with the FIP.
func TestScenario_FIPMainTableVethLeakException(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fipmainrule",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.FastDefaults()
	cfg.RouteTableID = intPtr(0) // Explicitly exercise the main-table path.
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.43"
	const rulePriority = testenv.DefaultVethLeakRulePriority - 1
	natUUID := testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.43")

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertIPRuleToPriorityTable(t, rulePriority, fip+"/32", 254, 10*time.Second)
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, "198.51.100.0/24", 10*time.Second)

	// Remove only the rule while retaining the /32 route. The periodic
	// reconcile must repair this exact upgrade/drift state without replacing
	// the healthy kernel route.
	testenv.DeleteIPRuleTo(t, rulePriority, fip+"/32")
	testenv.AssertNoIPRuleToPriority(t, rulePriority, fip+"/32", time.Second)
	testenv.AssertIPRuleToPriorityTable(t, rulePriority, fip+"/32", 254, 8*time.Second)

	testenv.RemoveFIP(t, ctx, nb, router, natUUID)
	testenv.AssertNoKernelRoute(t, fip, 15*time.Second)
	testenv.AssertNoIPRuleToPriority(t, rulePriority, fip+"/32", 15*time.Second)
}

// TestScenario_GatewaylessVirtualGateway (#42 scenario 2):
//
// A provider net without a real gateway (no pre-existing default route in NB)
// should cause the agent to:
//   - insert a 0.0.0.0/0 default route in NB pointing at the last usable host
//     IP of the LRP network (.254 for a /24)
//   - insert a Static_MAC_Binding for that virtual gateway IP, with MAC =
//     br-ex MAC
func TestScenario_GatewaylessVirtualGateway(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "gwless",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Last usable host IP of 198.51.100.0/24 is .254 (broadcast minus 1).
	const vgw = "198.51.100.254"

	testenv.Eventually(t, func() bool {
		_, ok := testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0")
		return ok
	}, 15*time.Second, 200*time.Millisecond, "agent must insert managed default route")

	route, _ := testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0")
	if route.Nexthop != vgw {
		t.Errorf("default route nexthop = %q, want %q", route.Nexthop, vgw)
	}
	if route.ExternalIDs["ovn-network-agent"] != "managed" {
		t.Errorf("default route is not tagged managed: %+v", route.ExternalIDs)
	}

	// MAC binding: lrp-gwless → 198.51.100.254 → bridge MAC.
	binding := testenv.EventuallyValue(t, func() (testenv.NBStaticMACBinding, bool) {
		return testenv.FindMACBinding(t, ctx, nb, router.LRPName, vgw)
	}, 15*time.Second, 200*time.Millisecond, "agent must insert static MAC binding for virtual gateway")
	if binding.MAC == "" {
		t.Fatalf("MAC binding has empty MAC")
	}

	// Verify the MAC matches br-ex's actual MAC.
	bridgeMAC := readBridgeMAC(t)
	if !strings.EqualFold(binding.MAC, bridgeMAC) {
		t.Errorf("MAC binding mac = %q, want bridge MAC %q", binding.MAC, bridgeMAC)
	}
}

// TestScenario_MultiRouterOnOneChassis (#42 scenario 3):
//
// Two routers active locally:
//   - both routers' FIPs end up as kernel + FRR routes
//   - the per-IP hairpin flow (cookie 0x998) is installed on br-ex for each
//     FIP, with mod_dl_dst set to the corresponding router-port MAC so OVN's
//     L2 lookup delivers the reflected packet to the right router
func TestScenario_MultiRouterOnOneChassis(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	r1 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "multia",
		LRPMAC:      "fa:16:3e:aa:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	r2 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "multib",
		LRPMAC:      "fa:16:3e:bb:00:02",
		LRPNetworks: []string{"203.0.113.11/24"},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		fipA = "198.51.100.55"
		fipB = "203.0.113.66"
	)
	testenv.AddFIP(t, ctx, nb, r1, fipA, "10.0.0.55")
	testenv.AddFIP(t, ctx, nb, r2, fipB, "10.0.1.66")

	// Both FIPs land in kernel + FRR.
	testenv.AssertKernelRoute(t, fipA, 15*time.Second)
	testenv.AssertKernelRoute(t, fipB, 15*time.Second)
	testenv.AssertFRRRoute(t, fipA, 15*time.Second)
	testenv.AssertFRRRoute(t, fipB, 15*time.Second)

	// Hairpin flow (cookie 0x998) installed for each FIP with mod_dl_dst
	// set to the *owning* router-port MAC. We assert by string-matching the
	// dump-flows output for the (nw_dst, mod_dl_dst) pair. ovs-ofctl
	// dump-flows reports IP-destination matches using the classic
	// `nw_dst=...` notation, not OpenFlow's `ip_dst=...`.
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fipA) && strings.Contains(line, "mod_dl_dst:"+r1.LRPMAC)
		}, 15*time.Second, "hairpin flow for FIP A → router A MAC")
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fipB) && strings.Contains(line, "mod_dl_dst:"+r2.LRPMAC)
		}, 15*time.Second, "hairpin flow for FIP B → router B MAC")
}

// TestScenario_HairpinFlowSelfHeal (#241):
//
// The differential apply has to be self-healing in one direction and
// completely still in the other. Both halves are checked here against real
// OVS:
//
//   - A flow that disappears out-of-band (someone runs del-flows, or OVS
//     restarts) is missing from the dump the next reconcile takes, lands in
//     that reconcile's add set, and is back within one interval.
//   - A flow that is already correct is never touched. The reconciles that
//     follow leave the installed entry alone, which shows up as a duration=
//     counter that keeps running instead of resetting to zero. The old
//     sweep-and-re-add reconcile reset it on every cycle, and with it the
//     packet counters this flow's traffic is debugged by.
func TestScenario_HairpinFlowSelfHeal(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "healr1",
		LRPMAC:      "fa:16:3e:cc:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	// A 2s reconcile interval, so the repair and the two quiet cycles after
	// it fit into a test without a long wait.
	const reconcileInterval = 2 * time.Second
	cfg := testenv.FastDefaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fip = "198.51.100.77"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.77")

	isFlow := func(line string) bool {
		return strings.Contains(line, "nw_dst="+fip) && strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
	}
	testenv.AssertOVSFlowMatches(t, "0x998", isFlow, 15*time.Second, "hairpin flow for the FIP")

	// Blow the plane away behind the agent's back.
	if out, err := exec.Command("ovs-ofctl", "del-flows", testenv.DefaultBridgeDev, "cookie=0x998/-1").CombinedOutput(); err != nil {
		t.Fatalf("out-of-band del-flows: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}

	// The next reconcile sees the flow missing and re-adds it.
	testenv.AssertOVSFlowMatches(t, "0x998", isFlow, 5*reconcileInterval, "hairpin flow repaired after an out-of-band delete")
	repaired := time.Now()

	// Now let two further reconciles pass with nothing changing in OVN. The
	// entry must survive them: its duration counts from the repair, so it
	// exceeds two intervals only if no reconcile replaced it in between.
	quiet := 3 * reconcileInterval
	time.Sleep(quiet)

	out, err := exec.Command("ovs-ofctl", "dump-flows", testenv.DefaultBridgeDev, "cookie=0x998/-1").CombinedOutput()
	if err != nil {
		t.Fatalf("dump-flows: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	var duration time.Duration
	for _, line := range strings.Split(string(out), "\n") {
		if !isFlow(line) {
			continue
		}
		duration = flowDuration(t, line)
	}
	if duration == 0 {
		t.Fatalf("hairpin flow for %s gone after %s of quiet reconciles; dump:\n%s", fip, quiet, strings.TrimSpace(string(out)))
	}
	if duration < 2*reconcileInterval {
		t.Errorf("hairpin flow duration = %s after %s (installed at %s), want more than two reconcile intervals: the flow was rewritten instead of left alone",
			duration, quiet, repaired.Format(time.TimeOnly))
	}
}

// flowDuration reads the duration= field ovs-ofctl reports for a flow, e.g.
// "duration=7.481s". It is how long the entry has been installed, so a
// reconcile that replaces the flow resets it.
func flowDuration(t *testing.T, line string) time.Duration {
	t.Helper()
	for _, field := range strings.Fields(strings.ReplaceAll(line, ",", " ")) {
		value, ok := strings.CutPrefix(field, "duration=")
		if !ok {
			continue
		}
		seconds, err := strconv.ParseFloat(strings.TrimSuffix(value, "s"), 64)
		if err != nil {
			t.Fatalf("unparseable duration %q in flow line %q: %v", value, line, err)
		}
		return time.Duration(seconds * float64(time.Second))
	}
	t.Fatalf("no duration= field in flow line %q", line)
	return 0
}
