//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// Dual-stack v4-guard scenario for issue #158 (tests a and b end to end).
//
// A dual-stack router carries both a v4 and a v6 network on its LRP, one v4
// FIP, one v6 FIP, and one NAT row with a malformed external_ip. The agent
// must:
//   - install the v4 FIP's kernel + FRR routes and stamp the takeover-readiness
//     marker (the v4 announce plane is unaffected by the v6 addresses)
//   - keep the v6 FIP's OVS hairpin flow (the hairpin plane stays dual-stack,
//     pinning #54's behaviour)
//   - never fail the FRR batch: the malformed row is dropped at ingest and the
//     v6 addresses are filtered out of the route/announce plane, so no
//     "failed to batch-add FRR routes" ever appears.
//
// Before #158, the v6 LRP gateway IP and v6 FIP reached AddFRRRoutes as invalid
// "ip route <v6>/32" commands, errored the whole batch, held announced=false,
// and blocked the marker — degrading every drain/takeover to the full timeout.
func TestScenario_DualStackV4Guard(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:   "dsguard",
		LRPMAC: "fa:16:3e:77:00:01",
		LRPNetworks: []string{
			"198.51.100.11/24",
			"2001:db8:cafe::11/64",
		},
	})

	cfg := testenv.Defaults()
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const (
		v4FIP = "198.51.100.55"
		v6FIP = "2001:db8:cafe::55"
	)
	testenv.AddFIP(t, ctx, nb, router, v4FIP, "10.0.0.55")
	testenv.AddFIP(t, ctx, nb, router, v6FIP, "fd00::55")
	// A malformed NAT row that must degrade exactly nothing.
	testenv.AddFIP(t, ctx, nb, router, "not-an-ip", "10.0.0.66")

	// The v4 FIP still announces via kernel + FRR despite the dual-stack router
	// and the malformed sibling row.
	testenv.AssertKernelRoute(t, v4FIP, 15*time.Second)
	testenv.AssertFRRRoute(t, v4FIP, 15*time.Second)

	// The v6 FIP keeps its OVS hairpin flow (dual-stack hairpin plane, #54).
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "ipv6_dst="+v6FIP) &&
				strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 15*time.Second, "v6 hairpin flow for FIP "+v6FIP)

	// The takeover-readiness marker is stamped on the managed default route:
	// proof the v4 announce succeeded (announced=true) even though the router
	// is dual-stack and one NAT row was malformed.
	local := testenv.LocalHostname(t)
	testenv.Eventually(t, func() bool {
		r, ok := testenv.FindStaticRoute(t, ctx, nb, router.RouterUUID, "0.0.0.0/0")
		return ok && r.ExternalIDs["ovn-network-agent-advertised"] == local
	}, 20*time.Second, 250*time.Millisecond,
		"agent must stamp the takeover-readiness marker for its v4 FIPs")

	// The malformed row and the v6 addresses never fail the FRR batch.
	if logs := a.LogTail(100000); strings.Contains(logs, "failed to batch-add FRR routes") {
		t.Errorf("FRR batch must not fail on the dual-stack/malformed input; last logs:\n%s", a.LogTail(60))
	}
}
