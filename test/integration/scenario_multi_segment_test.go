//go:build integration

package integration

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// multiSegmentCIDRs covers both provider networks used by the multi-segment
// scenarios. The failover and restart cases pass it as manual network_cidr:
// after one router leaves, auto-discovery no longer covers the departed
// network, and the agent's isManaged guard would then (deliberately) refuse
// to withdraw the departed FIP's FRR route. Operators running multiple
// provider networks are expected to list them, and so do these tests.
var multiSegmentCIDRs = []string{"198.51.100.0/24", "203.0.113.0/24"}

// readLinkMAC returns the MAC of the named interface. VLAN subinterfaces
// inherit the bridge MAC, so per-segment values are identical across
// segments — the assertions still resolve them per interface, mirroring
// what the agent does.
func readLinkMAC(t *testing.T, dev string) string {
	t.Helper()
	link, err := net.InterfaceByName(dev)
	if err != nil {
		t.Fatalf("lookup %s: %v", dev, err)
	}
	mac := link.HardwareAddr.String()
	if mac == "" {
		t.Fatalf("link %s has no MAC", dev)
	}
	return mac
}

// hasInPort reports whether a dump-flows line matches the given OpenFlow
// port. ovs-ofctl renders in_port as a number when its output is piped (as
// here) but as the port name on a terminal — tolerate both.
func hasInPort(line, ofport, patchPort string) bool {
	for _, tok := range []string{
		"in_port=" + ofport + ",",
		"in_port=" + ofport + " ",
		"in_port=" + patchPort + ",",
		"in_port=" + patchPort + " ",
		`in_port="` + patchPort + `"`,
	} {
		if strings.Contains(line, tok) {
			return true
		}
	}
	return false
}

// segmentPatchName is the br-ex side of the pair EnsureSegmentPatchPort creates.
func segmentPatchName(localnetPort string) string {
	return "patch-" + localnetPort + "-to-br-int"
}

// TestScenario_MultiSegmentTwoVLANNetworks (#147 AC 1, control/data plane):
//
// Two routers, each on its own VLAN provider network (tags 101/102 on the
// same physnet), both active on this chassis. The agent must create one
// kernel VLAN subinterface per segment, land each FIP's /32 on its own
// segment interface, install MAC-tweak and hairpin flows per patch port, and
// write each router's MAC binding at its own network's virtual gateway IP.
func TestScenario_MultiSegmentTwoVLANNetworks(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	tag101, tag102 := 101, 102
	testenv.EnsureSegmentPatchPort(t, "ln-v101")
	testenv.EnsureSegmentPatchPort(t, "ln-v102")

	r101 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "seg101",
		LRPMAC:      "fa:16:3e:01:01:01",
		LRPNetworks: []string{"198.51.100.11/24"},
		SegmentOpts: &testenv.SegmentOpts{LocalnetPort: "ln-v101", Tag: &tag101},
	})
	r102 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "seg102",
		LRPMAC:      "fa:16:3e:02:02:02",
		LRPNetworks: []string{"203.0.113.11/24"},
		SegmentOpts: &testenv.SegmentOpts{LocalnetPort: "ln-v102", Tag: &tag102},
	})

	const fip101 = "198.51.100.77"
	const fip102 = "203.0.113.77"
	testenv.AddFIP(t, ctx, nb, r101, fip101, "10.0.1.7")
	testenv.AddFIP(t, ctx, nb, r102, fip102, "10.0.2.7")

	a := readyAgent(t, testenv.Defaults())
	defer a.Stop(15 * time.Second)

	// One kernel VLAN subinterface per segment.
	testenv.AssertLinkExists(t, "br-ex.101", 20*time.Second)
	testenv.AssertLinkExists(t, "br-ex.102", 20*time.Second)

	// Each FIP's /32 lands on its own segment interface, and both are
	// announced via FRR.
	testenv.AssertKernelRouteOnDev(t, fip101, "br-ex.101", 20*time.Second)
	testenv.AssertKernelRouteOnDev(t, fip102, "br-ex.102", 20*time.Second)
	testenv.AssertFRRRoute(t, fip101, 20*time.Second)
	testenv.AssertFRRRoute(t, fip102, 20*time.Second)

	// MAC-tweak flows exist for both patch ports — two distinct in_ports,
	// which is exactly what the single-patch-port data path could not do.
	of101 := testenv.SegmentPatchOfport(t, "ln-v101")
	of102 := testenv.SegmentPatchOfport(t, "ln-v102")
	testenv.AssertOVSFlowMatches(t, "0x999",
		func(line string) bool { return hasInPort(line, of101, segmentPatchName("ln-v101")) },
		20*time.Second, "MAC-tweak flow on segment 101's patch port")
	testenv.AssertOVSFlowMatches(t, "0x999",
		func(line string) bool { return hasInPort(line, of102, segmentPatchName("ln-v102")) },
		20*time.Second, "MAC-tweak flow on segment 102's patch port")

	// Hairpin flows bind each FIP to its own segment's patch port with the
	// owning router-port MAC as mod_dl_dst.
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fip101) &&
				strings.Contains(line, "mod_dl_dst:"+r101.LRPMAC) &&
				hasInPort(line, of101, segmentPatchName("ln-v101"))
		}, 20*time.Second, "hairpin flow for FIP 101 on segment 101's patch port")
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fip102) &&
				strings.Contains(line, "mod_dl_dst:"+r102.LRPMAC) &&
				hasInPort(line, of102, segmentPatchName("ln-v102"))
		}, 20*time.Second, "hairpin flow for FIP 102 on segment 102's patch port")

	// Per-router MAC bindings at each network's virtual gateway IP (.254),
	// carrying the MAC of that router's own segment interface.
	mac101 := readLinkMAC(t, "br-ex.101")
	mac102 := readLinkMAC(t, "br-ex.102")
	testenv.Eventually(t, func() bool {
		b, ok := testenv.FindMACBinding(t, ctx, nb, r101.LRPName, "198.51.100.254")
		return ok && b.MAC == mac101
	}, 20*time.Second, 200*time.Millisecond,
		"MAC binding for router 101 at 198.51.100.254 with segment 101's interface MAC")
	testenv.Eventually(t, func() bool {
		b, ok := testenv.FindMACBinding(t, ctx, nb, r102.LRPName, "203.0.113.254")
		return ok && b.MAC == mac102
	}, 20*time.Second, 200*time.Millisecond,
		"MAC binding for router 102 at 203.0.113.254 with segment 102's interface MAC")

	// veth-leak routes cover both discovered provider networks.
	testenv.AssertVethRouteInVRF(t, "198.51.100.0/24", 20*time.Second)
	testenv.AssertVethRouteInVRF(t, "203.0.113.0/24", 20*time.Second)
}

// TestScenario_MultiSegmentFailoverIsolation (#147 AC 3):
//
// With two VLAN routers active, moving one router's chassisredirect port to
// a peer chassis must tear down only that router's segment resources —
// kernel VLAN interface, /32 routes, FRR announcement, MAC-tweak flow —
// while the other segment keeps serving untouched.
func TestScenario_MultiSegmentFailoverIsolation(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	tag101, tag102 := 101, 102
	testenv.EnsureSegmentPatchPort(t, "ln-v101")
	testenv.EnsureSegmentPatchPort(t, "ln-v102")

	r101 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fov101",
		LRPMAC:      "fa:16:3e:01:01:01",
		LRPNetworks: []string{"198.51.100.11/24"},
		SegmentOpts: &testenv.SegmentOpts{LocalnetPort: "ln-v101", Tag: &tag101},
	})
	r102 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fov102",
		LRPMAC:      "fa:16:3e:02:02:02",
		LRPNetworks: []string{"203.0.113.11/24"},
		SegmentOpts: &testenv.SegmentOpts{LocalnetPort: "ln-v102", Tag: &tag102},
	})

	const fip101 = "198.51.100.77"
	const fip102 = "203.0.113.77"
	testenv.AddFIP(t, ctx, nb, r101, fip101, "10.0.1.7")
	testenv.AddFIP(t, ctx, nb, r102, fip102, "10.0.2.7")

	cfg := testenv.Defaults()
	cfg.NetworkCIDRs = multiSegmentCIDRs
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Both segments fully in place before the failover, so the assertions
	// below prove removal rather than "never installed".
	testenv.AssertKernelRouteOnDev(t, fip101, "br-ex.101", 20*time.Second)
	testenv.AssertKernelRouteOnDev(t, fip102, "br-ex.102", 20*time.Second)
	testenv.AssertFRRRoute(t, fip101, 20*time.Second)
	testenv.AssertFRRRoute(t, fip102, 20*time.Second)

	of101 := testenv.SegmentPatchOfport(t, "ln-v101")
	of102 := testenv.SegmentPatchOfport(t, "ln-v102")

	// Move only router 101 to a peer chassis.
	peerChassis := testenv.MakeChassis(t, ctx, sb, "peer-host")
	testenv.SetCRPortChassis(t, ctx, sb, r101.CRPortUUID, &peerChassis)

	// Segment 101's resources are gone: VLAN interface pruned (taking its
	// /32 with it), FRR route withdrawn, flows removed from its patch port.
	testenv.AssertNoLink(t, "br-ex.101", 30*time.Second)
	testenv.AssertNoFRRRoute(t, fip101, 30*time.Second)
	testenv.AssertNoOVSFlowMatches(t, "0x999",
		func(line string) bool { return hasInPort(line, of101, segmentPatchName("ln-v101")) },
		30*time.Second, "MAC-tweak flow on departed segment 101's patch port")
	testenv.AssertNoOVSFlowMatches(t, "0x998",
		func(line string) bool { return strings.Contains(line, "nw_dst="+fip101) },
		30*time.Second, "hairpin flow for departed FIP 101")

	// Segment 102 is untouched.
	testenv.AssertLinkExists(t, "br-ex.102", 5*time.Second)
	testenv.AssertKernelRouteOnDev(t, fip102, "br-ex.102", 5*time.Second)
	testenv.AssertFRRRoute(t, fip102, 5*time.Second)
	testenv.AssertOVSFlowMatches(t, "0x999",
		func(line string) bool { return hasInPort(line, of102, segmentPatchName("ln-v102")) },
		5*time.Second, "MAC-tweak flow on surviving segment 102's patch port")
	mac102 := readLinkMAC(t, "br-ex.102")
	if b, ok := testenv.FindMACBinding(t, ctx, nb, r102.LRPName, "203.0.113.254"); !ok || b.MAC != mac102 {
		t.Fatalf("surviving router 102's MAC binding disturbed by failover (present=%v)", ok)
	}
}

// TestScenario_MultiSegmentSameSegmentHairpin (#147 AC 4, same-segment):
//
// Two routers sharing one VLAN segment (same localnet port, same external
// switch datapath): both FIPs' hairpin flows must land on the SAME patch
// port, each with its own router-port MAC, so same-segment cross-router
// hairpin reflection works. Also exercises the segment dedup path — one
// desired segment despite two routers.
func TestScenario_MultiSegmentSameSegmentHairpin(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	tag := 101
	testenv.EnsureSegmentPatchPort(t, "ln-shared")

	rA := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "sharea",
		LRPMAC:      "fa:16:3e:aa:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
		SegmentOpts: &testenv.SegmentOpts{LocalnetPort: "ln-shared", Tag: &tag},
	})
	rB := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "shareb",
		LRPMAC:      "fa:16:3e:bb:00:02",
		LRPNetworks: []string{"198.51.100.12/24"},
		// Attach to router A's external switch instead of creating a second
		// localnet segment.
		SegmentOpts: &testenv.SegmentOpts{DatapathUUID: rA.SegmentDatapathID},
	})

	const fipA = "198.51.100.81"
	const fipB = "198.51.100.82"
	testenv.AddFIP(t, ctx, nb, rA, fipA, "10.0.1.8")
	testenv.AddFIP(t, ctx, nb, rB, fipB, "10.0.2.8")

	a := readyAgent(t, testenv.Defaults())
	defer a.Stop(15 * time.Second)

	// One shared segment: one VLAN interface, both /32s on it.
	testenv.AssertLinkExists(t, "br-ex.101", 20*time.Second)
	testenv.AssertKernelRouteOnDev(t, fipA, "br-ex.101", 20*time.Second)
	testenv.AssertKernelRouteOnDev(t, fipB, "br-ex.101", 20*time.Second)

	// Both hairpin flows sit on the same patch port, each targeting its own
	// router's MAC.
	of := testenv.SegmentPatchOfport(t, "ln-shared")
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fipA) &&
				strings.Contains(line, "mod_dl_dst:"+rA.LRPMAC) &&
				hasInPort(line, of, segmentPatchName("ln-shared"))
		}, 20*time.Second, "hairpin flow for FIP A on the shared patch port")
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fipB) &&
				strings.Contains(line, "mod_dl_dst:"+rB.LRPMAC) &&
				hasInPort(line, of, segmentPatchName("ln-shared"))
		}, 20*time.Second, "hairpin flow for FIP B on the shared patch port")
}

// TestScenario_MultiSegmentAgentRestart (#147 AC 5):
//
// The agent stops without cleanup (cleanup_on_shutdown=false, simulating a
// restart that keeps routes in place). While it is down, one of the two
// routers disappears from NB. On restart the agent must re-adopt the
// surviving segment's VLAN interface and routes, and prune the departed
// segment's interface and FRR announcement.
func TestScenario_MultiSegmentAgentRestart(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	tag101, tag102 := 101, 102
	testenv.EnsureSegmentPatchPort(t, "ln-v101")
	testenv.EnsureSegmentPatchPort(t, "ln-v102")

	r101 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "rst101",
		LRPMAC:      "fa:16:3e:01:01:01",
		LRPNetworks: []string{"198.51.100.11/24"},
		SegmentOpts: &testenv.SegmentOpts{LocalnetPort: "ln-v101", Tag: &tag101},
	})
	r102 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "rst102",
		LRPMAC:      "fa:16:3e:02:02:02",
		LRPNetworks: []string{"203.0.113.11/24"},
		SegmentOpts: &testenv.SegmentOpts{LocalnetPort: "ln-v102", Tag: &tag102},
	})

	const fip101 = "198.51.100.77"
	const fip102 = "203.0.113.77"
	testenv.AddFIP(t, ctx, nb, r101, fip101, "10.0.1.7")
	testenv.AddFIP(t, ctx, nb, r102, fip102, "10.0.2.7")

	cfg := testenv.Defaults()
	cfg.NetworkCIDRs = multiSegmentCIDRs
	off := false
	cfg.CleanupOnShutdown = &off

	a := readyAgent(t, cfg)
	testenv.AssertKernelRouteOnDev(t, fip101, "br-ex.101", 20*time.Second)
	testenv.AssertKernelRouteOnDev(t, fip102, "br-ex.102", 20*time.Second)

	// Stop without cleanup: segment interfaces, routes, and flows survive.
	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}
	testenv.AssertLinkExists(t, "br-ex.101", 2*time.Second)
	testenv.AssertLinkExists(t, "br-ex.102", 2*time.Second)

	// While the agent is down, router 102 disappears from NB.
	deleteRouter(t, ctx, nb, r102.RouterUUID)

	a2 := readyAgent(t, cfg)
	defer a2.Stop(15 * time.Second)

	// The surviving segment is re-adopted: interface still there, /32 and
	// FRR announcement intact.
	testenv.AssertLinkExists(t, "br-ex.101", 20*time.Second)
	testenv.AssertKernelRouteOnDev(t, fip101, "br-ex.101", 20*time.Second)
	testenv.AssertFRRRoute(t, fip101, 20*time.Second)

	// The departed segment is pruned: agent-created interface removed
	// (taking its kernel route with it) and the stale FRR route withdrawn.
	testenv.AssertNoLink(t, "br-ex.102", 30*time.Second)
	testenv.AssertNoFRRRoute(t, fip102, 30*time.Second)
}
