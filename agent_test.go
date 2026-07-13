package main

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

func TestUniqueIPs(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{"nil input", nil, nil},
		{"empty input", []string{}, nil},
		{"single IP", []string{"10.0.0.1"}, []string{"10.0.0.1"}},
		{"duplicates removed", []string{"10.0.0.2", "10.0.0.1", "10.0.0.2"}, []string{"10.0.0.1", "10.0.0.2"}},
		{"sorted output", []string{"10.0.0.3", "10.0.0.1", "10.0.0.2"}, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}},
		{"whitespace trimmed", []string{" 10.0.0.1 ", "10.0.0.2\t"}, []string{"10.0.0.1", "10.0.0.2"}},
		{"empty strings filtered", []string{"", "10.0.0.1", "", "10.0.0.2", " "}, []string{"10.0.0.1", "10.0.0.2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uniqueIPs(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("uniqueIPs(%v) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestComputeEffectiveNetworks(t *testing.T) {
	_, manual, _ := net.ParseCIDR("10.0.0.0/24")
	_, discovered, _ := net.ParseCIDR("198.51.100.0/24")

	t.Run("manual config takes precedence", func(t *testing.T) {
		a := &Agent{cfg: Config{NetworkFilters: []*net.IPNet{manual}}}
		eff := a.computeEffectiveNetworks([]*net.IPNet{discovered})
		if len(eff) != 1 || eff[0].String() != "10.0.0.0/24" {
			t.Errorf("expected manual config, got %v", eff)
		}
	})

	t.Run("auto-discovery when no manual config", func(t *testing.T) {
		a := &Agent{cfg: Config{}}
		eff := a.computeEffectiveNetworks([]*net.IPNet{discovered})
		if len(eff) != 1 || eff[0].String() != "198.51.100.0/24" {
			t.Errorf("expected discovered network, got %v", eff)
		}
	})

	t.Run("nil when nothing configured or discovered", func(t *testing.T) {
		a := &Agent{cfg: Config{}}
		eff := a.computeEffectiveNetworks(nil)
		if len(eff) != 0 {
			t.Errorf("expected empty, got %v", eff)
		}
	})

	t.Run("drops IPv6 networks from a dual-stack discovered set", func(t *testing.T) {
		_, v6, _ := net.ParseCIDR("2001:db8::/64")
		a := &Agent{cfg: Config{}}
		eff := a.computeEffectiveNetworks([]*net.IPNet{discovered, v6})
		if len(eff) != 1 || eff[0].String() != "198.51.100.0/24" {
			t.Errorf("expected only the v4 network, got %v", eff)
		}
	})
}

func TestTriggerReconcile(t *testing.T) {
	a := &Agent{
		reconcileCh: make(chan struct{}, 1),
	}

	// First trigger should succeed.
	a.triggerReconcile()
	select {
	case <-a.reconcileCh:
		// ok
	default:
		t.Error("expected reconcile signal, got none")
	}

	// Second trigger without draining should not block.
	a.triggerReconcile()
	a.triggerReconcile() // Should not block even with full channel.
}

func TestVerifyRoutesDryRun(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// In dry-run mode, ListFRRRoutes and ListKernelRoutes return empty
	// lists (nil, nil). This means verifyRoutes sees every desired IP as
	// missing and attempts re-adds — but those are also dry-run no-ops.
	// This is by design: we exercise the full code path without side effects.
	n := a.verifyRoutes([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, nil, nil)
	if n != 6 { // 3 FRR + 3 kernel
		t.Errorf("expected 6 re-adds in dry-run, got %d", n)
	}
}

// TestVerifyRoutesVerifiesDesiredRegardlessOfFilters proves route ownership no
// longer comes from CIDR membership: a desired IP outside effectiveFilters
// (e.g. a port-forward VIP under a narrow manual network_cidr) is still
// verified and re-added, matching ensureRoutes which always installed it.
func TestVerifyRoutesVerifiesDesiredRegardlessOfFilters(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// Two IPs outside the manual filter. In dry-run every list call returns
	// empty, so each desired IP is seen as missing → 2 FRR + 2 kernel re-adds.
	n := a.verifyRoutes([]string{"192.168.1.1", "172.16.0.1"}, nil, nil)
	if n != 4 {
		t.Errorf("expected 4 re-adds for desired IPs outside the filter, got %d", n)
	}
}

func TestVerifyRoutesEmptyDesired(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	a := &Agent{routing: rm}

	// Empty desired list should be a no-op.
	if n := a.verifyRoutes(nil, nil, nil); n != 0 {
		t.Errorf("expected 0 re-adds for nil desired, got %d", n)
	}
	if n := a.verifyRoutes([]string{}, nil, nil); n != 0 {
		t.Errorf("expected 0 re-adds for empty desired, got %d", n)
	}
}

func TestVerifyRoutesConsecutiveReAddCounter(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// Simulate multiple consecutive cycles with missing routes (dry-run
	// always reports all routes as missing since list calls return nil).
	desired := []string{"10.0.0.1"}
	for i := 1; i <= 5; i++ {
		a.verifyRoutes(desired, nil, nil)
		if a.consecutiveReAdds != i {
			t.Errorf("after cycle %d: expected consecutiveReAdds=%d, got %d", i, i, a.consecutiveReAdds)
		}
	}

	// A cycle with an empty desired set (nothing to re-add) resets the counter.
	a.verifyRoutes(nil, nil, nil)
	if a.consecutiveReAdds != 0 {
		t.Errorf("expected consecutiveReAdds=0 after clean cycle, got %d", a.consecutiveReAdds)
	}
}

// TestEnsureRoutesDevMismatchDoesNotWithdrawFRR covers the (IP, Dev) model:
// a kernel route that sits on the wrong segment interface is re-replaced on
// the right device, but the IP is still desired — so its FRR announcement
// must NOT be withdrawn (a withdraw would blackhole the FIP during a segment
// move).
func TestEnsureRoutesDevMismatchDoesNotWithdrawFRR(t *testing.T) {
	rec := newVtyshRecorder()
	// FRR already has the desired IP.
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)
	rm := &RouteManager{
		bridgeDev:   "ovnagent-nonexistent-br",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		// The kernel route exists, but on the bridge device instead of the
		// segment's VLAN interface.
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
			return []kernelRouteEntry{{IP: "198.51.100.10", Dev: "ovnagent-nonexistent-br"}}, nil
		},
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	a.ensureRoutes([]string{"198.51.100.10"}, map[string]string{"198.51.100.10": "br-ex.101"}, nil)

	for _, c := range rec.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "no ip route 198.51.100.10/32") {
			t.Errorf("dev mismatch must not withdraw the FRR route, got: %v", rec.calls)
		}
		if strings.Contains(joined, "clear ip bgp") {
			t.Errorf("dev mismatch must not trigger a BGP refresh, got: %v", rec.calls)
		}
	}
}

// TestEnsureRoutesStaleEntryRemovedWithItsDevice verifies that a kernel
// route whose IP is no longer desired withdraws the FRR route regardless of
// which segment interface it sits on.
func TestEnsureRoutesStaleEntryRemovedWithItsDevice(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.99/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)
	rm := &RouteManager{
		bridgeDev:   "ovnagent-nonexistent-br",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
			return []kernelRouteEntry{{IP: "198.51.100.99", Dev: "br-ex.101"}}, nil
		},
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	a.ensureRoutes(nil, nil, nil)

	sawDel := false
	for _, c := range rec.calls {
		if strings.Contains(strings.Join(c, " "), "no ip route 198.51.100.99/32") {
			sawDel = true
		}
	}
	if !sawDel {
		t.Errorf("expected FRR withdrawal of stale 198.51.100.99, got calls: %v", rec.calls)
	}
}

// TestVerifyRoutesDetectsDevMismatch verifies that the post-change check
// treats a kernel route on the wrong device as missing and re-adds it on the
// segment's interface.
func TestVerifyRoutesDetectsDevMismatch(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)
	rm := &RouteManager{
		bridgeDev:   "ovnagent-nonexistent-br",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
			return []kernelRouteEntry{{IP: "198.51.100.10", Dev: "ovnagent-nonexistent-br"}}, nil
		},
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	// The route exists but on the wrong device — one kernel re-add expected
	// (the AddKernelRoute itself fails on the synthetic bridge, which is
	// fine: the count is what matters).
	n := a.verifyRoutes([]string{"198.51.100.10"}, map[string]string{"198.51.100.10": "br-ex.101"}, nil)
	if n != 1 {
		t.Errorf("expected 1 re-add for dev mismatch, got %d", n)
	}

	// With the device matching, no re-adds.
	n = a.verifyRoutes([]string{"198.51.100.10"}, nil, nil)
	if n != 0 {
		t.Errorf("expected 0 re-adds when device matches, got %d", n)
	}
}

// TestSegmentRouteUnresolved guards the decision at the heart of the transient
// segment-resolution fix: only a VLAN segment whose per-segment binding failed
// to resolve is skipped. A flat segment (bridge is correct), a resolved VLAN
// segment, and an unknown segment must all route normally — misclassifying any
// of them would either mis-deliver traffic or needlessly freeze a route.
func TestSegmentRouteUnresolved(t *testing.T) {
	tag := 101
	desired := map[string]DesiredSegment{
		"seg-101": {LocalnetPort: "seg-101", VLANTag: &tag}, // VLAN segment
		"":        {LocalnetPort: ""},                       // flat / fallback
	}
	rm := &RouteManager{bridgeDev: "br-ex", segments: map[string]*segmentBinding{}}
	a := &Agent{routing: rm}

	// VLAN segment with no binding this cycle → must be skipped.
	if !a.segmentRouteUnresolved("seg-101", desired) {
		t.Error("unresolved VLAN segment must be skipped")
	}
	// Flat segment routes on the bridge legitimately → never skipped.
	if a.segmentRouteUnresolved("", desired) {
		t.Error("flat segment must not be skipped")
	}
	// Unknown segment (not in the desired set) → route normally.
	if a.segmentRouteUnresolved("seg-999", desired) {
		t.Error("unknown segment must not be skipped")
	}
	// Once the VLAN segment resolves to its own binding → route normally.
	rm.segments["seg-101"] = &segmentBinding{kernelDev: "br-ex.101", vlanTag: &tag}
	if a.segmentRouteUnresolved("seg-101", desired) {
		t.Error("resolved VLAN segment must not be skipped")
	}
}

// TestVerifyRoutesLeavesUnresolvedVLANRouteInPlace is a regression guard for
// the transient failure that would otherwise relocate a VLAN FIP's /32 onto the
// untagged bridge. The FIP's kernel route sits on its VLAN subinterface
// (br-ex.101); its segment did not resolve this cycle, so the IP is flagged
// skip-kernel and ipDev carries no entry for it. verifyRoutes must leave the
// route untouched — zero re-adds — rather than re-add it on the bridge fallback
// (the exact move TestVerifyRoutesDetectsDevMismatch shows for an unskipped IP).
func TestVerifyRoutesLeavesUnresolvedVLANRouteInPlace(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)
	rm := &RouteManager{
		bridgeDev:   "ovnagent-nonexistent-br",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
			return []kernelRouteEntry{{IP: "198.51.100.10", Dev: "br-ex.101"}}, nil
		},
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	skip := map[string]bool{"198.51.100.10": true}
	if n := a.verifyRoutes([]string{"198.51.100.10"}, nil, skip); n != 0 {
		t.Errorf("unresolved VLAN segment must leave the FIP route in place, got %d re-adds", n)
	}
}

// TestReconcileNoLocalRoutersInvokesRemoveAllRoutes drives reconcile down
// the inactive-chassis branch: with no local routers and no port forwards,
// the agent calls removeAllRoutes("no locally active routers …") and
// cleans up veth-leak, prefix-list, and hairpin-flow state.
func TestReconcileNoLocalRoutersInvokesRemoveAllRoutes(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	// state.HasLocalRouters defaults to false; DiscoveredNetworks empty.
	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}
	a.reconcile(context.Background(), "test")
	// Reconcile must complete and leave effectiveFilters in a clean state.
	if len(a.effectiveFilters) != 0 {
		t.Errorf("effectiveFilters should be empty when no networks discovered, got %v", a.effectiveFilters)
	}
}

// TestReconcileWritesMarkerAfterSuccessfulAnnounce drives a dry-run reconcile
// with one local router active on this chassis and its managed default route
// carrying a peer's readiness marker. The announce succeeds (dry-run FRR/BGP
// never fail), so reconcile must stamp the local chassis into the marker so a
// draining peer learns this node has taken over. The nonexistent bridge makes
// GetBridgeMAC fail, so EnsureGatewayRouting writes nothing and the marker
// update is the only NB write.
func TestReconcileWritesMarkerAfterSuccessfulAnnounce(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "ovnagent-nonexistent-br",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.state.LocalRouters = []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1"}}
	c.state.HasLocalRouters = true
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", Name: "r1", StaticRoutes: []string{"sr1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "sr1",
		IPPrefix: "0.0.0.0/0",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-b",
			takeoverReadyMarkerKey:      "host-b",
		},
	})

	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}
	a.reconcile(context.Background(), "test")

	if !hasMarkerUpdate(nb.writeTransacts(), "host-a") {
		t.Fatalf("reconcile did not stamp the takeover marker for host-a; writes: %v", nb.writeTransacts())
	}
}

// TestReconcileMixedFamilyAnnouncesV4AndWritesMarker drives a non-dry-run
// reconcile for a router whose NAT set carries both a v4 and a v6 FIP (issue
// #158 test a). The v4 FIP must be announced via vtysh and the v6 string must
// never reach a vtysh command, so the FRR batch never fails on an IPv6 route —
// which lets the announce succeed and the takeover marker be stamped. Without
// the family guard the v6 FIP would reach AddFRRRoutes as an "ip route
// 2001:db8::50/32" batch, error the whole batch, and withhold the marker.
//
// The v4 FIP's kernel route is pre-seeded via listKernelRoutesHook so
// AddKernelRoute (Linux-only) is never invoked; the OVS hook errors so segment
// discovery bails cleanly (hairpin reconcile becomes a no-op) — neither path
// affects the announce outcome under test.
func TestReconcileMixedFamilyAnnouncesV4AndWritesMarker(t *testing.T) {
	const (
		v4FIP  = "198.51.100.50"
		v6FIP  = "2001:db8::50"
		bridge = "ovnagent-nonexistent-br"
		lrpMAC = "fa:16:3e:aa:aa:aa"
	)
	rec := newVtyshRecorder()
	rm := &RouteManager{
		bridgeDev:     bridge,
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
		execOVSHook: func(*exec.Cmd) ([]byte, error) {
			return nil, errors.New("test: no ovs available")
		},
		// The v4 FIP's /32 already exists on the bridge, so ensureRoutes sees
		// no missing kernel route and never calls the Linux-only AddKernelRoute.
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
			return []kernelRouteEntry{{IP: v4FIP, Dev: bridge}}, nil
		},
	}

	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.state.LocalRouters = []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1", LRPMAC: lrpMAC}}
	c.state.HasLocalRouters = true
	c.state.NATIPToRouterMAC = map[string]string{v4FIP: lrpMAC, v6FIP: lrpMAC}
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", Name: "r1", StaticRoutes: []string{"sr1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "sr1",
		IPPrefix: "0.0.0.0/0",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-b",
			takeoverReadyMarkerKey:      "host-b",
		},
	})

	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}
	a.reconcile(context.Background(), "test")

	var joined string
	for _, call := range rec.calls {
		joined += " " + strings.Join(call, " ")
	}
	if !strings.Contains(joined, "ip route "+v4FIP+"/32 169.254.0.1") {
		t.Errorf("v4 FIP was not announced via vtysh; calls: %v", rec.calls)
	}
	if strings.Contains(joined, v6FIP) {
		t.Errorf("v6 FIP must never reach a vtysh command; calls: %v", rec.calls)
	}
	if !hasMarkerUpdate(nb.writeTransacts(), "host-a") {
		t.Fatalf("reconcile did not stamp the takeover marker for host-a; writes: %v", nb.writeTransacts())
	}
}

// TestReconcileSkipsMarkerWithoutLocalRouters proves the HasLocalRouters gate:
// with no locally-active routers the node is not a takeover candidate, so even
// though NB holds a managed default route, reconcile must not stamp a marker.
func TestReconcileSkipsMarkerWithoutLocalRouters(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "ovnagent-nonexistent-br",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	// HasLocalRouters defaults to false.
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", Name: "r1", StaticRoutes: []string{"sr1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "sr1",
		IPPrefix: "0.0.0.0/0",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-b",
			takeoverReadyMarkerKey:      "host-b",
		},
	})

	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}
	a.reconcile(context.Background(), "test")

	if hasMarkerUpdate(nb.writeTransacts(), "host-a") {
		t.Fatalf("reconcile wrote a takeover marker with no local routers; writes: %v", nb.writeTransacts())
	}
}

// TestReconcileFailedAnnounceWithholdsMarker pins the failure side of the
// takeover-readiness gate (agent.go: `announced && state.HasLocalRouters`).
// A node that owns local routers but whose announce fails — here the FRR
// batch add errors, so ensureRoutes returns false — must NOT stamp the
// readiness marker, because it has attracted BGP traffic it cannot deliver.
// A draining peer polls that marker before releasing its own FIP routes, so a
// premature marker would blackhole traffic during the HA window this handshake
// exists to protect.
//
// This is the negative twin of TestReconcileMixedFamilyAnnouncesV4AndWritesMarker
// (which drives a successful announce and asserts the marker IS written) and of
// TestReconcileSkipsMarkerWithoutLocalRouters (which pins the HasLocalRouters
// half). Dropping the `announced` conjunct — the mutation `if
// state.HasLocalRouters` — makes this test fail: the marker would be written
// despite the failed announce.
func TestReconcileFailedAnnounceWithholdsMarker(t *testing.T) {
	const (
		fip    = "198.51.100.50"
		bridge = "ovnagent-nonexistent-br"
		lrpMAC = "fa:16:3e:aa:aa:aa"
	)
	rec := newVtyshRecorder()
	// Arm the FRR batch-add for the FIP to fail. AddFRRRoutes joins the batch
	// errors and returns non-nil, so ensureRoutes sets announced = false.
	rec.on([]string{"vtysh", "-c", "conf t", "-c", "vrf vrf-provider",
		"-c", "ip route " + fip + "/32 169.254.0.1", "-c", "exit-vrf", "-c", "end"},
		"", errors.New("test: vtysh add failed"))
	rm := &RouteManager{
		bridgeDev:     bridge,
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
		execOVSHook: func(*exec.Cmd) ([]byte, error) {
			return nil, errors.New("test: no ovs available")
		},
		// Pre-seed the FIP /32 on the bridge so ensureRoutes never calls the
		// Linux-only AddKernelRoute — the announce fails purely on the FRR add.
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
			return []kernelRouteEntry{{IP: fip, Dev: bridge}}, nil
		},
	}

	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.state.LocalRouters = []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1", LRPMAC: lrpMAC}}
	c.state.HasLocalRouters = true
	c.state.NATIPToRouterMAC = map[string]string{fip: lrpMAC}
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", Name: "r1", StaticRoutes: []string{"sr1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "sr1",
		IPPrefix: "0.0.0.0/0",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-b",
			takeoverReadyMarkerKey:      "host-b",
		},
	})

	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}
	a.reconcile(context.Background(), "test")

	// Vacuity guard: the failing announce path must actually have been driven —
	// the FRR add was attempted for the FIP.
	attempted := false
	for _, call := range rec.calls {
		if strings.Contains(strings.Join(call, " "), "ip route "+fip+"/32 169.254.0.1") {
			attempted = true
			break
		}
	}
	if !attempted {
		t.Fatalf("FRR add for %s was never attempted; calls: %v", fip, rec.calls)
	}

	if hasMarkerUpdate(nb.writeTransacts(), "host-a") {
		t.Fatalf("reconcile stamped the takeover marker after a failed announce; writes: %v", nb.writeTransacts())
	}
}

// hasMarkerUpdate reports whether any recorded write updates a
// Logical_Router_Static_Route external_ids with a takeover readiness marker
// naming chassis.
func hasMarkerUpdate(writes [][]ovsdb.Operation, chassis string) bool {
	for _, batch := range writes {
		for _, op := range batch {
			if op.Op != ovsdb.OperationUpdate || op.Table != "Logical_Router_Static_Route" {
				continue
			}
			ext, ok := op.Row["external_ids"].(map[string]string)
			if ok && ext[takeoverReadyMarkerKey] == chassis {
				return true
			}
		}
	}
	return false
}

// TestRemoveAllRoutesDryRun exercises removeAllRoutes end-to-end. In dry-run
// mode List* helpers return (nil, nil) so the function walks every branch
// (FRR list, kernel list, BGP refresh skipped because no routes to remove).
func TestRemoveAllRoutesDryRun(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	a := &Agent{routing: rm}
	a.removeAllRoutes("test reason")
}

// TestEnsureRoutesAddsMissingAndRemovesStale drives ensureRoutes with a
// vtysh hook so the FRR add/delete paths and the BGP-refresh-on-delete
// branch all execute. Kernel route helpers fail (bridge does not exist or
// platform is non-Linux), which is expected and survives as a logged
// warning — that path is itself exercised.
func TestEnsureRoutesAddsMissingAndRemovesStale(t *testing.T) {
	rec := newVtyshRecorder()
	// FRR currently has B (already desired) and stale-X (managed but not desired).
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.20/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
S>* 198.51.100.99/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)

	rm := &RouteManager{
		bridgeDev:     "ovnagent-nonexistent-br",
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	// Desired: 198.51.100.10 (new) and 198.51.100.20 (already in FRR).
	a.ensureRoutes([]string{"198.51.100.10", "198.51.100.20"}, nil, nil)

	var sawAdd, sawDel, sawRefresh bool
	for _, c := range rec.calls {
		joined := strings.Join(c, " ")
		switch {
		case strings.Contains(joined, "show ip route vrf"):
			continue
		case strings.Contains(joined, "ip route 198.51.100.10/32 169.254.0.1") &&
			!strings.Contains(joined, "no ip route"):
			sawAdd = true
		case strings.Contains(joined, "no ip route 198.51.100.99/32"):
			sawDel = true
		case strings.Contains(joined, "clear ip bgp vrf vrf-provider"):
			sawRefresh = true
		}
	}
	if !sawAdd {
		t.Errorf("expected add of 198.51.100.10, got calls: %v", rec.calls)
	}
	if !sawDel {
		t.Errorf("expected del of stale 198.51.100.99, got calls: %v", rec.calls)
	}
	if !sawRefresh {
		t.Errorf("expected BGP refresh after deletes, got calls: %v", rec.calls)
	}
}

// TestEnsureRoutesReturnsAnnounceOutcome pins the announce outcome the drain
// takeover handshake keys on: ensureRoutes returns true when the FIP routes
// were announced (or nothing needed changing) and false when the FRR add or
// the BGP soft-refresh failed. PortForwardOnly skips kernel routes so only the
// FRR/BGP paths — the ones the outcome depends on — are exercised, keeping the
// test off the real bridge.
func TestEnsureRoutesReturnsAnnounceOutcome(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	newAgent := func(rec *vtyshRecorder) *Agent {
		rm := &RouteManager{
			bridgeDev:     "ovnagent-nonexistent-br",
			vrfName:       "vrf-provider",
			vethNexthop:   "169.254.0.1",
			execVtyshHook: rec.hook(),
		}
		return &Agent{cfg: Config{PortForwardOnly: true}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}
	}

	t.Run("clean add announces", func(t *testing.T) {
		rec := newVtyshRecorder()
		// FRR reports no routes, so the desired IP is added and BGP refreshed;
		// the recorder returns success for both.
		if !newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, nil, nil) {
			t.Errorf("clean add cycle: announced = false, want true (calls: %v)", rec.calls)
		}
	})

	t.Run("no-change cycle announces", func(t *testing.T) {
		rec := newVtyshRecorder()
		// FRR already advertises the only desired IP: nothing is added or
		// removed, so no BGP refresh runs and the cycle still announces.
		rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
			"S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01\n", nil)
		if !newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, nil, nil) {
			t.Errorf("no-change cycle: announced = false, want true (calls: %v)", rec.calls)
		}
	})

	t.Run("FRR add failure does not announce", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", "conf t", "-c", "vrf vrf-provider",
			"-c", "ip route 198.51.100.10/32 169.254.0.1", "-c", "exit-vrf", "-c", "end"},
			"", errors.New("vtysh add failed"))
		if newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, nil, nil) {
			t.Errorf("FRR add failure: announced = true, want false")
		}
	})

	t.Run("BGP refresh failure does not announce", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", "clear ip bgp vrf vrf-provider * soft out"},
			"", errors.New("bgp refresh failed"))
		if newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, nil, nil) {
			t.Errorf("BGP refresh failure: announced = true, want false")
		}
	})
}

// inactiveRoutesGauge reads the single-series ovn_network_agent_inactive_routes
// gauge from the test registry.
func inactiveRoutesGauge(t *testing.T, m *metricsRegistry) float64 {
	t.Helper()
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_inactive_routes" {
			continue
		}
		series := mf.GetMetric()
		if len(series) == 0 {
			t.Fatalf("inactive_routes gauge has no series")
		}
		return series[0].GetGauge().GetValue()
	}
	t.Fatalf("metric ovn_network_agent_inactive_routes not found in registry")
	return 0
}

// TestCheckFRRRouteActivity pins the two uncovered branches of
// checkFRRRouteActivity (agent.go). The documented contract is that a failed
// vtysh check leaves the inactive_routes gauge at its previous value rather
// than falsely resetting it to zero, and that a configured-but-inactive route
// bumps the gauge and logs an alert. Deleting the early return on the error
// path — which would zero the alerting metric during a vtysh outage — is
// caught by the "vtysh error keeps gauge" subtest.
func TestCheckFRRRouteActivity(t *testing.T) {
	const fip = "198.51.100.10"
	const jsonCmd = "show ip route vrf vrf-provider static json"
	newAgent := func(rec *vtyshRecorder) *Agent {
		rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
		return &Agent{routing: rm, cfg: Config{VRFName: "vrf-provider"}}
	}

	t.Run("vtysh error keeps gauge", func(t *testing.T) {
		m := withTestMetrics(t)
		logs := captureSlog(t)
		// A prior cycle observed one inactive route. The failing check must
		// not clobber that value with a false zero.
		setInactiveRoutes(3)

		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", jsonCmd}, "", errors.New("test: vtysh unreachable"))
		newAgent(rec).checkFRRRouteActivity([]string{fip})

		if got := inactiveRoutesGauge(t, m); got != 3 {
			t.Errorf("gauge after failed check = %v, want 3 (unchanged)", got)
		}
		if !strings.Contains(logs.String(), "could not verify FRR route activity") {
			t.Errorf("expected the check-failure warning; logs:\n%s", logs.String())
		}
	})

	t.Run("inactive route sets gauge and logs alert", func(t *testing.T) {
		m := withTestMetrics(t)
		logs := captureSlog(t)

		rec := newVtyshRecorder()
		// selected/installed are omitted (FRR drops them when false), so the
		// route is configured but not advertised.
		rec.on([]string{"vtysh", "-c", jsonCmd},
			`{"`+fip+`/32":[{"prefix":"`+fip+`/32","protocol":"static"}]}`, nil)
		newAgent(rec).checkFRRRouteActivity([]string{fip})

		if got := inactiveRoutesGauge(t, m); got != 1 {
			t.Errorf("gauge with one inactive route = %v, want 1", got)
		}
		if !strings.Contains(logs.String(), "FRR static routes are configured but inactive") {
			t.Errorf("expected the inactive-route alert; logs:\n%s", logs.String())
		}
	})

	t.Run("healthy route resets gauge", func(t *testing.T) {
		m := withTestMetrics(t)
		setInactiveRoutes(3)

		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", jsonCmd},
			`{"`+fip+`/32":[{"prefix":"`+fip+`/32","protocol":"static","selected":true,"installed":true}]}`, nil)
		newAgent(rec).checkFRRRouteActivity([]string{fip})

		if got := inactiveRoutesGauge(t, m); got != 0 {
			t.Errorf("gauge with a healthy route = %v, want 0", got)
		}
	})
}

// TestRemoveAllRoutesWithStubbedFRRList exercises the FRR-driven removal
// path: a stub vtysh hook reports two managed routes, the agent batches
// the deletion, and a BGP soft-refresh follows because routes were removed.
func TestRemoveAllRoutesWithStubbedFRRList(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
S>* 198.51.100.11/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)
	rm := &RouteManager{
		// Use a synthetic bridge name that does not exist on either macOS
		// or Linux CI hosts so ListKernelRoutes errors out (netlink) or
		// returns "only supported on Linux" (stub) instead of touching real
		// kernel state. dryRun is intentionally false here because the
		// FRR-list short-circuits to nil in dry-run mode and would skip
		// the code path we want to exercise.
		bridgeDev:     "ovnagent-nonexistent-br",
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	a.removeAllRoutes("test")

	// Expect: list FRR (1), batch delete (1), BGP soft-refresh (1).
	var sawDel, sawRefresh bool
	for _, c := range rec.calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "no ip route 198.51.100.10/32") &&
			strings.Contains(joined, "no ip route 198.51.100.11/32") {
			sawDel = true
		}
		if strings.Contains(joined, "clear ip bgp vrf vrf-provider") {
			sawRefresh = true
		}
	}
	if !sawDel {
		t.Errorf("expected batched delete of both managed IPs, got calls: %v", rec.calls)
	}
	if !sawRefresh {
		t.Errorf("expected BGP soft-refresh after deletes, got calls: %v", rec.calls)
	}
}

// TestCleanupRunsShutdownPipeline drives the agent's cleanup() in dry-run
// mode so each step (FRR routes, prefix-list, OVS flows, routing table,
// port forwards, veth leak, bridge IP, OVN managed entries) executes without
// touching real system state. The OVN nb client is a fake so the final
// RemoveManagedNBEntries call uses the in-memory rows.
func TestCleanupRunsShutdownPipeline(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	a := &Agent{
		cfg:     Config{BridgeIP: "169.254.169.254"},
		ovn:     c,
		routing: rm,
	}
	// Must not panic; all sub-calls are dry-run no-ops or interact with the
	// fake OVN client (no in-memory routers → RemoveManagedNBEntries early returns).
	a.cleanup()
}

// TestCleanupStaleChassis_TracksAndPrunes verifies the full tracking flow:
// (1) a chassis referenced by a managed route but missing from allChassis is
// added to missingChassis; (2) a chassis that returns is removed; (3) a
// chassis no longer referenced is pruned without waiting for grace.
func TestCleanupStaleChassis_TracksAndPrunes(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Logical_Router_Static_Route",
		&NBLogicalRouterStaticRoute{
			UUID: "r-dead",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-gone",
			},
		},
		&NBLogicalRouterStaticRoute{
			UUID: "r-alive",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-a",
			},
		},
	)

	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: 5 * time.Minute},
		ovn:            c,
		missingChassis: make(map[string]time.Time),
	}

	// First call: host-gone is missing, host-a is alive.
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})
	if _, tracked := a.missingChassis["host-gone"]; !tracked {
		t.Errorf("expected host-gone to be tracked as missing, got %v", a.missingChassis)
	}
	if _, tracked := a.missingChassis["host-a"]; tracked {
		t.Errorf("host-a is alive and must not be tracked as missing")
	}

	// Second call: host-gone returns; it must be removed from tracking.
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true, "host-gone": true})
	if _, tracked := a.missingChassis["host-gone"]; tracked {
		t.Error("host-gone returned and must be removed from missingChassis")
	}

	// Third call: nb has only routes for host-a (host-gone route was deleted
	// elsewhere). missingChassis must be pruned even though grace would still apply.
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID: "r-alive",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-a",
		},
	})
	a.missingChassis["stale-record"] = time.Now() // synthetic stale entry
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})
	if _, tracked := a.missingChassis["stale-record"]; tracked {
		t.Error("stale-record is unreferenced and must be pruned")
	}
}

// TestCleanupStaleChassis_TriggersCleanupAfterGrace verifies that a chassis
// missing for longer than the configured grace period causes the agent to
// call CleanupStaleChassisManagedEntries against the OVN client.
func TestCleanupStaleChassis_TriggersCleanupAfterGrace(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"r-stale"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID: "r-stale",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-gone",
		},
	})

	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: time.Millisecond},
		ovn:            c,
		missingChassis: make(map[string]time.Time),
	}
	// Pre-seed the tracker so the grace period has already elapsed.
	a.missingChassis["host-gone"] = time.Now().Add(-time.Hour)

	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})

	tx := nb.recordedTransacts()
	if len(tx) == 0 {
		t.Fatal("expected CleanupStaleChassisManagedEntries to issue at least one transact")
	}
	// host-gone should be removed from the tracker after successful cleanup.
	if _, tracked := a.missingChassis["host-gone"]; tracked {
		t.Error("host-gone should be removed from missingChassis after grace-period cleanup")
	}
}

// TestCleanupStaleChassis_BailsOnListError verifies that an OVN list error
// short-circuits cleanupStaleChassis: it returns early and does not mutate
// missingChassis state.
func TestCleanupStaleChassis_BailsOnListError(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.listErr = errors.New("connection refused")

	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: time.Minute},
		ovn:            c,
		missingChassis: make(map[string]time.Time),
	}
	a.cleanupStaleChassis(context.Background(), map[string]bool{"host-a": true})
	if len(a.missingChassis) != 0 {
		t.Errorf("missingChassis should not be mutated on list error, got %v", a.missingChassis)
	}
}

func TestCleanupStaleChassisDisabledWhenZero(t *testing.T) {
	a := &Agent{
		cfg:            Config{StaleChassisGracePeriod: 0},
		missingChassis: make(map[string]time.Time),
	}

	// Should return immediately without touching missingChassis.
	a.cleanupStaleChassis(context.Background(), map[string]bool{"node-1": true})

	if len(a.missingChassis) != 0 {
		t.Errorf("expected empty missingChassis when grace period is 0, got %v", a.missingChassis)
	}
}

func TestNewAgentInitializesMissingChassis(t *testing.T) {
	cfg := Config{
		VethNexthop: "169.254.0.1",
		VRFName:     "vrf-provider",
	}
	a, err := NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent() error: %v", err)
	}
	if a.missingChassis == nil {
		t.Error("missingChassis should be initialized")
	}
	if len(a.missingChassis) != 0 {
		t.Errorf("missingChassis should be empty, got %v", a.missingChassis)
	}
	if a.staleCleanupJitter < 0 || a.staleCleanupJitter > maxStaleCleanupJitter {
		t.Errorf("staleCleanupJitter = %v, should be in [0, %v]", a.staleCleanupJitter, maxStaleCleanupJitter)
	}
}

// TestConnectWithRetry_SucceedsFirstTry verifies the happy path: when connect
// returns nil on the first call, the helper returns nil immediately without
// sleeping for retryInterval.
func TestConnectWithRetry_SucceedsFirstTry(t *testing.T) {
	var calls atomic.Int32
	connect := func(context.Context) error {
		calls.Add(1)
		return nil
	}

	start := time.Now()
	if err := connectWithRetry(context.Background(), connect, time.Hour); err != nil {
		t.Fatalf("connectWithRetry: unexpected error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("connect calls = %d, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("returned in %s, expected near-instant on first-try success", elapsed)
	}
}

// TestConnectWithRetry_RetriesOnFailure verifies the retry path: when connect
// fails several times, the helper keeps calling it without returning the
// error, until it eventually succeeds. The retry interval is set short so
// the test stays cheap.
func TestConnectWithRetry_RetriesOnFailure(t *testing.T) {
	var calls atomic.Int32
	const wantCalls = 3
	connect := func(context.Context) error {
		if calls.Add(1) < wantCalls {
			return errors.New("connection refused")
		}
		return nil
	}

	if err := connectWithRetry(context.Background(), connect, 10*time.Millisecond); err != nil {
		t.Fatalf("connectWithRetry: unexpected error: %v", err)
	}
	if got := calls.Load(); got != wantCalls {
		t.Errorf("connect calls = %d, want %d", got, wantCalls)
	}
}

// TestConnectWithRetry_ReturnsCtxErrOnCancel verifies that a context cancelled
// while the helper is in the retry-wait branch yields ctx.Err() (not a
// generic timeout, not a panic, not a swallowed error). This is the
// production contract that lets a SIGTERM during cold-start retry exit
// cleanly.
func TestConnectWithRetry_ReturnsCtxErrOnCancel(t *testing.T) {
	var calls atomic.Int32
	connect := func(context.Context) error {
		calls.Add(1)
		return errors.New("always fails")
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after entering the retry-wait branch; the retry
	// interval is long enough that the helper is definitely waiting on
	// either ctx.Done or the timer when the cancel fires.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := connectWithRetry(ctx, connect, 10*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("connectWithRetry err = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("helper took %s to honour cancel — should return promptly", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("connect calls = %d, want 1 (one failed attempt before cancel)", got)
	}
}

func TestMissingChassisGracePeriodTracking(t *testing.T) {
	// Directly test the missingChassis map tracking logic.
	a := &Agent{
		cfg:                Config{StaleChassisGracePeriod: 5 * time.Minute},
		missingChassis:     make(map[string]time.Time),
		staleCleanupJitter: 0, // No jitter for deterministic testing.
	}

	now := time.Now()

	// Simulate: chassis "dead-node" was first seen missing 6 minutes ago.
	a.missingChassis["dead-node"] = now.Add(-6 * time.Minute)

	// Simulate: chassis "rebooting-node" was first seen missing 2 minutes ago.
	a.missingChassis["rebooting-node"] = now.Add(-2 * time.Minute)

	effectiveGrace := a.cfg.StaleChassisGracePeriod + a.staleCleanupJitter

	// Check which chassis have exceeded the grace period.
	var stale []string
	for name, firstSeen := range a.missingChassis {
		if now.Sub(firstSeen) >= effectiveGrace {
			stale = append(stale, name)
		}
	}

	if len(stale) != 1 || stale[0] != "dead-node" {
		t.Errorf("expected only dead-node to be stale, got %v", stale)
	}

	// Simulate: rebooting-node comes back.
	allChassis := map[string]bool{"rebooting-node": true}
	for name := range a.missingChassis {
		if allChassis[name] {
			delete(a.missingChassis, name)
		}
	}
	if _, tracked := a.missingChassis["rebooting-node"]; tracked {
		t.Error("rebooting-node should have been removed from missingChassis after returning")
	}
	if _, tracked := a.missingChassis["dead-node"]; !tracked {
		t.Error("dead-node should still be in missingChassis")
	}
}

// reconcile takes ctx and passes it to OVN methods that issue OVSDB
// transactions, but it does not loop or block, so a pre-cancelled context
// should never make it hang or panic — it should simply complete promptly
// while the ctx-aware OVN calls observe the cancel and return early. With
// a dry-run RouteManager and a fake OVN client carrying populated state,
// the test exercises the full reconcile body to lock in that "context
// cancellation while a reconcile is in flight" stays a no-side-effects
// fast-return, not a partial-write hang.
func TestReconcileCompletesPromptlyOnCancelledContext(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	// Populate state so reconcile takes the HasLocalRouters branch and
	// therefore reaches the ctx-aware EnsureGatewayRouting /
	// EnsureActivePriorityLead calls.
	c.state.LocalRouters = []LocalRouterInfo{
		{
			RouterName:  "router1",
			RouterUUID:  "lr-1",
			LRPName:     "lrp-abc",
			LRPMAC:      "aa:aa:aa:aa:aa:aa",
			LRPNetworks: []string{"198.51.100.0/24"},
		},
	}
	c.state.HasLocalRouters = true
	c.state.DiscoveredNetworks = []*net.IPNet{cidr}

	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE reconcile starts — the strict "mid-cycle" case

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		// reconcile must not panic even with a cancelled ctx.
		a.reconcile(ctx, "test")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("reconcile did not return within 2s on cancelled ctx — possible hang")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reconcile returned in %s, expected near-instant on cancelled ctx", elapsed)
	}

	// effectiveFilters is recomputed each cycle and is the only piece of
	// agent state mutated by reconcile. With HasLocalRouters=true and a
	// discovered /24, the slice must be set — i.e. reconcile completed its
	// state-derivation phase rather than aborting mid-flight in a way that
	// would leave the agent half-initialised on the next tick.
	if len(a.effectiveFilters) != 1 || a.effectiveFilters[0].String() != "198.51.100.0/24" {
		t.Errorf("effectiveFilters = %v, want [198.51.100.0/24]", a.effectiveFilters)
	}
}

// portForwardOnlyConfig returns a minimal port-forward-only config (no OVN
// remotes) suitable for the agent tests below.
func portForwardOnlyConfig() Config {
	return Config{
		PortForwardOnly:    true,
		PortForwardEnabled: true,
		DryRun:             true,
		CleanupOnShutdown:  true,
		DrainOnShutdown:    true, // must be ignored: there is no OVN to drain
		ReconcileInterval:  time.Hour,
		VethNexthop:        "169.254.0.1",
		VRFName:            "vrf-provider",
		PortForwards: []PortForwardVIP{
			{
				VIP:       "198.51.100.10",
				ManageVIP: true,
				Rules:     []PortForwardRule{{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"}},
			},
		},
	}
}

// TestNewAgentPortForwardOnlyHasNoOVNClient verifies that NewAgent does not
// construct an OVN client in port-forward-only mode — the agent runs as a
// standalone VIP service with no OVN connection.
func TestNewAgentPortForwardOnlyHasNoOVNClient(t *testing.T) {
	a, err := NewAgent(portForwardOnlyConfig())
	if err != nil {
		t.Fatalf("NewAgent() error: %v", err)
	}
	if a.ovn != nil {
		t.Error("OVN client must not be created in port-forward-only mode")
	}
}

// TestReconcilePortForwardOnly drives a reconcile cycle with no OVN client.
// The cycle must complete without panicking on the nil client, derive an
// empty OVN state, and leave effectiveFilters empty (nothing discovered).
func TestReconcilePortForwardOnly(t *testing.T) {
	cfg := portForwardOnlyConfig()
	rm := NewRouteManager(cfg)
	a := &Agent{
		cfg:            cfg,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	a.reconcile(context.Background(), "test")

	if len(a.effectiveFilters) != 0 {
		t.Errorf("effectiveFilters should be empty in port-forward-only mode, got %v", a.effectiveFilters)
	}
}

// TestCleanupPortForwardOnly verifies that cleanup() runs the port-forward
// teardown path without dereferencing the nil OVN client (OVN NB cleanup is
// skipped in port-forward-only mode).
func TestCleanupPortForwardOnly(t *testing.T) {
	cfg := portForwardOnlyConfig()
	a := &Agent{
		cfg:     cfg,
		routing: NewRouteManager(cfg),
	}
	// Must not panic: a.ovn is nil and RemoveManagedNBEntries must be skipped.
	a.cleanup()
}

// TestAgentRunPortForwardOnly exercises Run end-to-end in port-forward-only
// mode with a pre-cancelled context: the agent starts (veth/port-forward
// setup, startup reconcile), then the cancelled context drives it straight
// into shutdown cleanup. It must never touch OVN and must return nil.
func TestAgentRunPortForwardOnly(t *testing.T) {
	cfg := portForwardOnlyConfig()
	cfg.VethLeakEnabled = true

	a, err := NewAgent(cfg)
	if err != nil {
		t.Fatalf("NewAgent() error: %v", err)
	}
	if a.ovn != nil {
		t.Fatal("OVN client must not be created in port-forward-only mode")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shut down immediately after the startup reconcile

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return within 2s in port-forward-only mode")
	}
}
