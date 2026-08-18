package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// TestComputeLeakNetworks pins the #258 gate: the leak plane claims only
// locally-owned networks, with the manual list acting as a filter over them —
// never as the set itself.
func TestComputeLeakNetworks(t *testing.T) {
	parse := func(cidrs ...string) []*net.IPNet {
		var out []*net.IPNet
		for _, s := range cidrs {
			_, n, err := net.ParseCIDR(s)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, n)
		}
		return out
	}

	tests := []struct {
		name       string
		cfg        Config
		discovered []*net.IPNet
		want       []string
	}{
		{"auto mode leaks the discovered set",
			Config{}, parse("198.51.100.0/24"), []string{"198.51.100.0/24"}},
		{"manual filter drops the un-owned networks",
			Config{NetworkFilters: parse("192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24")},
			parse("198.51.100.0/24"), []string{"198.51.100.0/24"}},
		{"broader manual filter keeps the owned network",
			Config{NetworkFilters: parse("198.51.0.0/16")},
			parse("198.51.100.0/24"), []string{"198.51.100.0/24"}},
		{"disjoint manual filter leaks nothing",
			Config{NetworkFilters: parse("10.0.0.0/24")},
			parse("198.51.100.0/24"), nil},
		{"manual filter narrower than the owned network excludes it",
			Config{NetworkFilters: parse("198.51.100.0/25")},
			parse("198.51.100.0/24"), nil},
		{"IPv6 networks are dropped",
			Config{}, parse("198.51.100.0/24", "2001:db8::/64"), []string{"198.51.100.0/24"}},
		{"port-forward-only leaks the manual list, ownership unknowable",
			Config{PortForwardOnly: true, NetworkFilters: parse("192.0.2.0/24")},
			nil, []string{"192.0.2.0/24"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{cfg: tt.cfg}
			got := netStrings(a.computeLeakNetworks(tt.discovered))
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("computeLeakNetworks = %v, want %v", got, tt.want)
			}
		})
	}
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
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// In dry-run mode, ListFRRRoutes and ListKernelRoutes return empty
	// lists (nil, nil). This means verifyRoutes sees every desired IP as
	// missing and attempts re-adds — but those are also dry-run no-ops.
	// This is by design: we exercise the full code path without side effects.
	n := a.verifyRoutes([]string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, nil, nil)
	if n != 6 { // 3 FRR + 3 kernel
		t.Errorf("expected 6 re-adds in dry-run, got %d", n)
	}
}

// TestVerifyRoutesVerifiesDesiredRegardlessOfFilters proves route ownership no
// longer comes from CIDR membership: a desired IP outside effectiveFilters
// (e.g. a port-forward VIP under a narrow manual network_cidr) is still
// verified and re-added, matching ensureRoutes which always installed it.
func TestVerifyRoutesVerifiesDesiredRegardlessOfFilters(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// Two IPs outside the manual filter. In dry-run every list call returns
	// empty, so each desired IP is seen as missing → 2 FRR + 2 kernel re-adds.
	n := a.verifyRoutes([]string{"192.168.1.1", "172.16.0.1"}, []string{"192.168.1.1", "172.16.0.1"}, nil, nil)
	if n != 4 {
		t.Errorf("expected 4 re-adds for desired IPs outside the filter, got %d", n)
	}
}

func TestVerifyRoutesEmptyDesired(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	a := &Agent{routing: rm}

	// Empty desired list should be a no-op.
	if n := a.verifyRoutes(nil, nil, nil, nil); n != 0 {
		t.Errorf("expected 0 re-adds for nil desired, got %d", n)
	}
	if n := a.verifyRoutes([]string{}, nil, nil, nil); n != 0 {
		t.Errorf("expected 0 re-adds for empty desired, got %d", n)
	}
}

func TestVerifyRoutesConsecutiveReAddCounter(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	a := &Agent{
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
	}

	// Simulate multiple consecutive cycles with missing routes (dry-run
	// always reports all routes as missing since list calls return nil).
	desired := []string{"10.0.0.1"}
	for i := 1; i <= 5; i++ {
		a.verifyRoutes(desired, desired, nil, nil)
		if a.consecutiveReAdds != i {
			t.Errorf("after cycle %d: expected consecutiveReAdds=%d, got %d", i, i, a.consecutiveReAdds)
		}
	}

	// A cycle with an empty desired set (nothing to re-add) resets the counter.
	a.verifyRoutes(nil, nil, nil, nil)
	if a.consecutiveReAdds != 0 {
		t.Errorf("expected consecutiveReAdds=0 after clean cycle, got %d", a.consecutiveReAdds)
	}
}

// unresolvableNexthopAgent builds an agent in the #214 failure state: the FIP
// statics are configured in FRR but none entered the RIB, so every cycle sees
// them missing and re-adds them. connected controls what zebra reports as
// connected in the VRF — the fixture's only variable, and the one that decides
// whether the agent's own next-hop is the cause.
//
// PortForwardOnly keeps verifyRoutes off the kernel plane: the failure is
// FRR-side, and the agent must not be driven at netlink from a unit test.
func unresolvableNexthopAgent(t *testing.T, connected string) (*Agent, *[][]*net.IPNet) {
	t.Helper()

	rec := newVtyshRecorder()
	// No static route is in the RIB — an unresolvable next-hop keeps all of
	// them out of it, which is what makes verifyRoutes re-add forever.
	rec.on(strings.Fields("vtysh -c show ip route vrf vrf-provider static json"), "{}", nil)
	rec.on(strings.Fields("vtysh -c show ip route vrf vrf-provider connected json"), connected, nil)

	rm := &RouteManager{cfg: Config{
		BridgeDev:       "br-ex",
		VRFName:         "vrf-provider",
		VethNexthop:     "169.254.0.1",
		VethProviderIP:  "169.254.0.2",
		VethLeakEnabled: true,
		PortForwardOnly: true,
	}}
	rm.execVtyshHook = rec.hook()

	var refreshed [][]*net.IPNet
	rm.refreshVethNexthopHook = func(networks []*net.IPNet) error {
		refreshed = append(refreshed, networks)
		return nil
	}

	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	return &Agent{
		cfg:              rm.cfg,
		routing:          rm,
		effectiveFilters: []*net.IPNet{cidr},
		// In port-forward-only mode the leak set is the manual list, so the
		// reconcile would compute the same value (computeLeakNetworks).
		leakNetworks: []*net.IPNet{cidr},
	}, &refreshed
}

// TestVerifyRoutesRepairsUnresolvableNexthop is the regression test for #214.
// Before the fix the agent detected this state and retried the one action that
// could not fix it, forever. It must now re-notify the kernel about the
// veth-provider address so zebra relearns the connected route — but only after
// the instability has persisted, and only once per cooldown, because the flap
// briefly withdraws the VRF's routes through the veth.
func TestVerifyRoutesRepairsUnresolvableNexthop(t *testing.T) {
	// zebra holds the uplink's connected route but not the veth /30.
	a, refreshed := unresolvableNexthopAgent(t, frrConnectedRoutesJSON("100.64.1.0/30"))
	desired := []string{"192.0.2.1"}

	// Below the threshold the fault is not yet established: a single missing
	// route is an ordinary race and must not cost an address flap.
	for i := 1; i < consecutiveReAddThreshold; i++ {
		a.verifyRoutes(desired, desired, nil, nil)
		if len(*refreshed) != 0 {
			t.Fatalf("repair ran after %d cycle(s), want none before the threshold of %d", i, consecutiveReAddThreshold)
		}
	}

	a.verifyRoutes(desired, desired, nil, nil)
	if len(*refreshed) != 1 {
		t.Fatalf("repair ran %d times at the threshold, want 1", len(*refreshed))
	}
	if len((*refreshed)[0]) != 1 {
		t.Errorf("repair got %d networks, want the agent's 1 leak network", len((*refreshed)[0]))
	}

	// Still broken on the next cycle: the diagnosis repeats, the flap does not.
	a.verifyRoutes(desired, desired, nil, nil)
	if len(*refreshed) != 1 {
		t.Errorf("repair ran %d times, want 1 — the cooldown must suppress the second attempt", len(*refreshed))
	}

	// Once the cooldown has elapsed the agent tries again: a flap that did not
	// take must not leave the data plane down for good.
	a.lastNexthopRepair = time.Now().Add(-2 * nexthopRepairCooldown)
	a.verifyRoutes(desired, desired, nil, nil)
	if len(*refreshed) != 2 {
		t.Errorf("repair ran %d times after the cooldown elapsed, want 2", len(*refreshed))
	}
}

// TestVerifyRoutesLeavesResolvableNexthopAlone is the other half of the
// contract. Persistent re-adds with a next-hop that does resolve mean something
// else is deleting the routes; flapping the address would not stop it and would
// take the veth's routes down for no reason.
func TestVerifyRoutesLeavesResolvableNexthopAlone(t *testing.T) {
	a, refreshed := unresolvableNexthopAgent(t, frrConnectedRoutesJSON("169.254.0.0/30", "100.64.1.0/30"))
	desired := []string{"192.0.2.1"}

	for i := 0; i < consecutiveReAddThreshold+2; i++ {
		a.verifyRoutes(desired, desired, nil, nil)
	}
	if a.consecutiveReAdds < consecutiveReAddThreshold {
		t.Fatalf("consecutiveReAdds = %d, want the instability detector to have fired", a.consecutiveReAdds)
	}
	if len(*refreshed) != 0 {
		t.Errorf("repair ran %d times with a resolvable next-hop, want 0", len(*refreshed))
	}
}

// TestRepairUnresolvableNexthopSkipsWhenVethLeakDisabled proves the repair stays
// out of deployments that have no veth to flap — there the next-hop is the
// operator's own and the agent has no business touching it.
func TestRepairUnresolvableNexthopSkipsWhenVethLeakDisabled(t *testing.T) {
	a, refreshed := unresolvableNexthopAgent(t, frrConnectedRoutesJSON("100.64.1.0/30"))
	a.cfg.VethLeakEnabled = false

	for i := 0; i < consecutiveReAddThreshold+1; i++ {
		a.verifyRoutes([]string{"192.0.2.1"}, []string{"192.0.2.1"}, nil, nil)
	}
	if len(*refreshed) != 0 {
		t.Errorf("repair ran %d times with veth leak disabled, want 0", len(*refreshed))
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
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "198.51.100.10"),
		nil,
	)
	rm := &RouteManager{
		cfg: Config{
			BridgeDev:   "ovnagent-nonexistent-br",
			VRFName:     "vrf-provider",
			VethNexthop: "169.254.0.1",
		},
		// The kernel route exists, but on the bridge device instead of the
		// segment's VLAN interface.
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
			return []kernelRouteEntry{{IP: "198.51.100.10", Dev: "ovnagent-nonexistent-br"}}, nil
		},
		execVtyshHook: rec.hook(),
	}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	a.ensureRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, map[string]string{"198.51.100.10": "br-ex.101"}, nil)

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
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "198.51.100.99"),
		nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
		return []kernelRouteEntry{{IP: "198.51.100.99", Dev: "br-ex.101"}}, nil
	}, execVtyshHook: rec.hook()}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	a.ensureRoutes(nil, nil, nil, nil)

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
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "198.51.100.10"),
		nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
		return []kernelRouteEntry{{IP: "198.51.100.10", Dev: "ovnagent-nonexistent-br"}}, nil
	}, execVtyshHook: rec.hook()}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	// The route exists but on the wrong device — one kernel re-add expected
	// (the AddKernelRoute itself fails on the synthetic bridge, which is
	// fine: the count is what matters).
	n := a.verifyRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, map[string]string{"198.51.100.10": "br-ex.101"}, nil)
	if n != 1 {
		t.Errorf("expected 1 re-add for dev mismatch, got %d", n)
	}

	// With the device matching, no re-adds.
	n = a.verifyRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, nil, nil)
	if n != 0 {
		t.Errorf("expected 0 re-adds when device matches, got %d", n)
	}
}

// TestSegmentRouteUnresolved guards the decision at the heart of the transient
// segment-resolution fix: only a VLAN segment whose per-segment binding failed
// to resolve is skipped. A flat segment (bridge is correct), a resolved VLAN
// segment, and an unknown segment must all route normally — misclassifying any
// of them would either mis-deliver traffic or needlessly freeze a route.
// TestVIPRoutesAnnounceable pins the #206 gate: a port-forward VIP only gets a
// route where the agent also maintains the FRR prefix-list that would permit it.
func TestVIPRoutesAnnounceable(t *testing.T) {
	tests := []struct {
		name            string
		portForwardOnly bool
		hasLocalRouters bool
		want            bool
	}{
		// The defect: no local routers means reconcile empties the
		// prefix-list, so a VIP route could never be advertised.
		{"gateway without local routers", false, false, false},
		{"gateway with local routers", false, true, true},
		// Port-forward-only mode never takes the standby path and serves
		// its VIPs without any OVN state at all.
		{"port-forward-only", true, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{cfg: Config{PortForwardOnly: tt.portForwardOnly}}
			got := a.vipRoutesAnnounceable(OVNState{HasLocalRouters: tt.hasLocalRouters})
			if got != tt.want {
				t.Errorf("vipRoutesAnnounceable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReportDormantVIPsLogsOnChange is the other half of the #206 fix: the
// dormancy is reported when the set changes, not on every reconcile. The old
// behaviour logged at ERROR every cycle for as long as the condition held.
func TestReportDormantVIPsLogsOnChange(t *testing.T) {
	a := &Agent{}

	a.reportDormantVIPs([]string{"192.0.2.99"})
	if a.dormantVIPs != "192.0.2.99" {
		t.Fatalf("dormantVIPs = %q, want the reported set memoized", a.dormantVIPs)
	}
	// Same set again — nothing changes, so nothing is re-reported.
	a.reportDormantVIPs([]string{"192.0.2.99"})
	if a.dormantVIPs != "192.0.2.99" {
		t.Errorf("dormantVIPs = %q, want unchanged on a repeat", a.dormantVIPs)
	}
	// A second VIP joining is a change and must be picked up.
	a.reportDormantVIPs([]string{"192.0.2.99", "203.0.113.10"})
	if a.dormantVIPs != "192.0.2.99,203.0.113.10" {
		t.Errorf("dormantVIPs = %q, want the grown set", a.dormantVIPs)
	}
	// Recovery clears the memo so a later dormancy is reported again.
	a.reportDormantVIPs(nil)
	if a.dormantVIPs != "" {
		t.Errorf("dormantVIPs = %q, want cleared on recovery", a.dormantVIPs)
	}
}

// countLevel returns how many records in a captured slog buffer were emitted at
// the given level.
func countLevel(buf *bytes.Buffer, level string) int {
	return strings.Count(buf.String(), "level="+level+" ")
}

// A missing VRF default route is loud in proportion to what the node has riding
// on it: an outage where VIPs are announced, a narrower limitation where only
// OVN clients egress, and nothing at all on a node using neither (#247).
func TestReportVRFDefaultRouteLogsBySeverity(t *testing.T) {
	tests := []struct {
		name            string
		present         bool
		checkErr        error
		announcedVIPs   int
		hasLocalRouters bool
		wantLevel       string
		wantState       string
	}{
		{
			name: "absent while announcing VIPs is an outage",
			// A VIP whose replies cannot be routed is dark for every client
			// outside the VRF's own networks.
			announcedVIPs: 2, hasLocalRouters: true,
			wantLevel: "ERROR", wantState: vrfDefaultOutage,
		},
		{
			name:            "absent while only hosting routers is a limitation",
			hasLocalRouters: true,
			wantLevel:       "WARN", wantState: vrfDefaultLimited,
		},
		{
			name: "absent with nothing riding on it is silent",
			// A standby announcing nothing and hosting nothing loses no
			// traffic to the missing route; the gauge still records it.
			wantLevel: "", wantState: vrfDefaultIdle,
		},
		{
			name:     "an unanswerable check warns rather than claiming absence",
			checkErr: errors.New("find VRF vrf-provider: no such network interface"),
			// The VIPs would make this an outage if the answer were known.
			announcedVIPs: 2, hasLocalRouters: true,
			wantLevel: "WARN", wantState: vrfDefaultUnknown,
		},
		{
			name:    "present is silent on a node that never reported a problem",
			present: true, hasLocalRouters: true,
			wantLevel: "", wantState: vrfDefaultPresent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSlog(t)
			a := &Agent{cfg: Config{VRFName: "vrf-provider"}}

			a.reportVRFDefaultRoute(tc.present, tc.checkErr, tc.announcedVIPs, tc.hasLocalRouters)

			if a.vrfDefaultRoute != tc.wantState {
				t.Errorf("vrfDefaultRoute = %q, want %q", a.vrfDefaultRoute, tc.wantState)
			}
			for _, level := range []string{"ERROR", "WARN", "INFO"} {
				want := 0
				if level == tc.wantLevel {
					want = 1
				}
				if got := countLevel(buf, level); got != want {
					t.Errorf("%s lines = %d, want %d; log was:\n%s", level, got, want, buf.String())
				}
			}
		})
	}
}

// The condition persists for as long as the fabric withholds the route, so it
// is reported on entry and on recovery — never once per reconcile.
func TestReportVRFDefaultRouteLogsOncePerTransition(t *testing.T) {
	buf := captureSlog(t)
	a := &Agent{cfg: Config{VRFName: "vrf-provider"}}

	for range 3 {
		a.reportVRFDefaultRoute(false, nil, 1, true)
	}
	if got := countLevel(buf, "ERROR"); got != 1 {
		t.Errorf("ERROR lines = %d, want exactly 1 across three identical cycles; log was:\n%s", got, buf.String())
	}

	// The route coming back pairs the outage with a resolution.
	a.reportVRFDefaultRoute(true, nil, 1, true)
	if got := countLevel(buf, "INFO"); got != 1 {
		t.Errorf("INFO lines = %d, want the recovery reported once; log was:\n%s", got, buf.String())
	}
	if a.vrfDefaultRoute != vrfDefaultPresent {
		t.Errorf("vrfDefaultRoute = %q, want %q", a.vrfDefaultRoute, vrfDefaultPresent)
	}
}

// A node that takes on routers while the route is still missing has entered a
// reportable condition, even though the route itself did not change. Keying the
// memo on the route alone would keep that silent forever.
func TestReportVRFDefaultRouteReportsAChangeOfStake(t *testing.T) {
	buf := captureSlog(t)
	a := &Agent{cfg: Config{VRFName: "vrf-provider"}}

	a.reportVRFDefaultRoute(false, nil, 0, false)
	if got := countLevel(buf, "WARN"); got != 0 {
		t.Fatalf("WARN lines = %d, want none while nothing rides on the route; log was:\n%s", got, buf.String())
	}

	a.reportVRFDefaultRoute(false, nil, 0, true)
	if got := countLevel(buf, "WARN"); got != 1 {
		t.Errorf("WARN lines = %d, want the node gaining routers reported; log was:\n%s", got, buf.String())
	}

	a.reportVRFDefaultRoute(false, nil, 3, true)
	if got := countLevel(buf, "ERROR"); got != 1 {
		t.Errorf("ERROR lines = %d, want the node starting to announce reported; log was:\n%s", got, buf.String())
	}
}

// The gauge tracks the check every cycle, but a failed check must leave the
// last real answer standing rather than report a zero it never established.
func TestReportVRFDefaultRouteGauge(t *testing.T) {
	m := withTestMetrics(t)
	captureSlog(t)
	a := &Agent{cfg: Config{VRFName: "vrf-provider"}}

	a.reportVRFDefaultRoute(true, nil, 1, true)
	if got := gaugeValue(t, m, "ovn_network_agent_vrf_default_route_present"); got != 1 {
		t.Errorf("gauge = %v, want 1 while the route is present", got)
	}

	a.reportVRFDefaultRoute(false, nil, 0, false)
	if got := gaugeValue(t, m, "ovn_network_agent_vrf_default_route_present"); got != 0 {
		t.Errorf("gauge = %v, want 0 on a silent absence", got)
	}

	a.reportVRFDefaultRoute(true, nil, 1, true)
	a.reportVRFDefaultRoute(false, errors.New("boom"), 1, true)
	if got := gaugeValue(t, m, "ovn_network_agent_vrf_default_route_present"); got != 1 {
		t.Errorf("gauge = %v, want the last known answer held across a failed check", got)
	}
}

func TestSegmentRouteUnresolved(t *testing.T) {
	tag := 101
	desired := map[string]DesiredSegment{
		"seg-101": {LocalnetPort: "seg-101", VLANTag: &tag}, // VLAN segment
		"":        {LocalnetPort: ""},                       // flat / fallback
	}
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: map[string]*segmentBinding{}}
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
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "198.51.100.10"),
		nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
		return []kernelRouteEntry{{IP: "198.51.100.10", Dev: "br-ex.101"}}, nil
	}, execVtyshHook: rec.hook()}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	skip := map[string]bool{"198.51.100.10": true}
	if n := a.verifyRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, nil, skip); n != 0 {
		t.Errorf("unresolved VLAN segment must leave the FIP route in place, got %d re-adds", n)
	}
}

// TestReconcileNoLocalRoutersInvokesRemoveAllRoutes drives reconcile down
// the inactive-chassis branch: with no local routers and no port forwards,
// the agent calls removeAllRoutes("no locally active routers …") and
// cleans up veth-leak, prefix-list, and hairpin-flow state.
func TestReconcileNoLocalRoutersInvokesRemoveAllRoutes(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
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
	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.state.Replace(OVNState{
		LocalRouters:    []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1"}},
		HasLocalRouters: true,
	})
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

// TestReconcileWithdrawsPreUpgradeVIPStaticAndAddsNoVIPStatic pins the
// announce-path switch (#223). A port-forward VIP is advertised through its
// connected route on port_forward_dev, never an FRR static — the static a
// pre-upgrade agent wrote is permanently shadowed by that connected route and
// so was never advertised. On the first cycle after the upgrade the agent must
// therefore (a) issue no `ip route <vip>/32` add — the VIP is no longer in the
// FRR-static set — and (b) withdraw the stale static through the orphan path
// that compares ListFRRRoutes against FRRStaticIPs. The VIP's kernel /32 is
// pre-seeded so AddKernelRoute is never called and the route is kept.
//
// Both modes are covered: full mode, where the VIP is announceable because a
// local router is active, and port-forward-only mode, where FRRStaticIPs is
// empty so every listed static is an orphan and withdrawn.
func TestReconcileWithdrawsPreUpgradeVIPStaticAndAddsNoVIPStatic(t *testing.T) {
	const (
		vip    = "203.0.113.10"
		bridge = "ovnagent-nonexistent-br"
	)

	assertNoAddButWithdraw := func(t *testing.T, rec *vtyshRecorder) {
		t.Helper()
		var added, withdrew bool
		for _, call := range rec.calls {
			joined := strings.Join(call, " ")
			if strings.Contains(joined, "ip route "+vip+"/32") && !strings.Contains(joined, "no ip route") {
				added = true
			}
			if strings.Contains(joined, "no ip route "+vip+"/32") {
				withdrew = true
			}
		}
		if added {
			t.Errorf("a VIP must never get an FRR static (it announces through its connected route); calls: %v", rec.calls)
		}
		if !withdrew {
			t.Errorf("the pre-upgrade VIP static must be withdrawn on the first cycle; calls: %v", rec.calls)
		}
	}

	newRM := func(rec *vtyshRecorder) *RouteManager {
		return &RouteManager{
			cfg:           Config{BridgeDev: bridge, VRFName: "vrf-provider", VethNexthop: "169.254.0.1"},
			execVtyshHook: rec.hook(),
			execOVSHook: func(*exec.Cmd) ([]byte, error) {
				return nil, errors.New("test: no ovs available")
			},
			// The VIP's /32 already sits on the bridge, so ensureRoutes never
			// calls the Linux-only AddKernelRoute and the kernel route is kept.
			listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
				return []kernelRouteEntry{{IP: vip, Dev: bridge}}, nil
			},
		}
	}

	seedStatic := func(rec *vtyshRecorder) {
		// A pre-upgrade agent left the VIP's static via the agent's veth nexthop.
		rec.on(
			[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
			frrStaticRoutesJSON("169.254.0.1", vip),
			nil,
		)
	}

	t.Run("full mode", func(t *testing.T) {
		rec := newVtyshRecorder()
		seedStatic(rec)
		c, _, _ := newOVNClientWithFakes(t, "host-a")
		c.state.Replace(OVNState{
			LocalRouters:    []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1"}},
			HasLocalRouters: true,
		})
		a := &Agent{
			cfg:            Config{BridgeDev: bridge, PortForwards: []PortForwardVIP{{VIP: vip, ManageVIP: true}}},
			ovn:            c,
			routing:        newRM(rec),
			reconcileCh:    make(chan struct{}, 1),
			missingChassis: make(map[string]time.Time),
		}
		a.reconcile(context.Background(), "test")
		assertNoAddButWithdraw(t, rec)
	})

	t.Run("port-forward-only mode", func(t *testing.T) {
		rec := newVtyshRecorder()
		seedStatic(rec)
		rm := newRM(rec)
		rm.cfg.PortForwardOnly = true
		a := &Agent{
			cfg:            Config{PortForwardOnly: true, BridgeDev: bridge, PortForwards: []PortForwardVIP{{VIP: vip, ManageVIP: true}}},
			routing:        rm,
			reconcileCh:    make(chan struct{}, 1),
			missingChassis: make(map[string]time.Time),
		}
		a.reconcile(context.Background(), "test")
		assertNoAddButWithdraw(t, rec)
	})
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
		cfg: Config{
			BridgeDev:   bridge,
			VRFName:     "vrf-provider",
			VethNexthop: "169.254.0.1",
		},
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
	c.state.Replace(OVNState{
		LocalRouters:     []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1", LRPMAC: lrpMAC}},
		HasLocalRouters:  true,
		NATIPToRouterMAC: map[string]string{v4FIP: lrpMAC, v6FIP: lrpMAC},
	})
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
	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
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
		cfg: Config{
			BridgeDev:   bridge,
			VRFName:     "vrf-provider",
			VethNexthop: "169.254.0.1",
		},
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
	c.state.Replace(OVNState{
		LocalRouters:     []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1", LRPMAC: lrpMAC}},
		HasLocalRouters:  true,
		NATIPToRouterMAC: map[string]string{fip: lrpMAC},
	})
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

// TestReconcileRecordsReadinessOutcome pins the /readyz feed: reconcile stamps
// the last-reconcile outcome via setLastReconcileStatus(cycleOK), where cycleOK
// tracks routeSyncResult.ready. A successful announce and a healthy standby
// cycle both report ready; a failed announce reports not-ready.
func TestReconcileRecordsReadinessOutcome(t *testing.T) {
	// a. Successful announce — modelled on
	// TestReconcileWritesMarkerAfterSuccessfulAnnounce. Dry-run FRR/BGP never
	// fail, so ensureRoutes reports ready and the cycle is OK.
	t.Run("successful announce is ready", func(t *testing.T) {
		m := withTestMetrics(t)
		rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
		c, nb, _ := newOVNClientWithFakes(t, "host-a")
		c.state.Replace(OVNState{
			LocalRouters:    []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1"}},
			HasLocalRouters: true,
		})
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

		if !m.readiness.reconcileRan.Load() {
			t.Error("reconcileRan not set after a completed reconcile")
		}
		if !m.readiness.lastReconcileOK.Load() {
			t.Error("lastReconcileOK false after a successful announce")
		}
	})

	// b. Failed announce — modelled on TestReconcileFailedAnnounceWithholdsMarker.
	// The FRR batch add errors, so ensureRoutes reports not-ready.
	t.Run("failed announce is not ready", func(t *testing.T) {
		m := withTestMetrics(t)
		const (
			fip    = "198.51.100.50"
			bridge = "ovnagent-nonexistent-br"
			lrpMAC = "fa:16:3e:aa:aa:aa"
		)
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", "conf t", "-c", "vrf vrf-provider",
			"-c", "ip route " + fip + "/32 169.254.0.1", "-c", "exit-vrf", "-c", "end"},
			"", errors.New("test: vtysh add failed"))
		rm := &RouteManager{
			cfg: Config{
				BridgeDev:   bridge,
				VRFName:     "vrf-provider",
				VethNexthop: "169.254.0.1",
			},
			execVtyshHook: rec.hook(),
			execOVSHook: func(*exec.Cmd) ([]byte, error) {
				return nil, errors.New("test: no ovs available")
			},
			listKernelRoutesHook: func() ([]kernelRouteEntry, error) {
				return []kernelRouteEntry{{IP: fip, Dev: bridge}}, nil
			},
		}

		c, nb, _ := newOVNClientWithFakes(t, "host-a")
		c.state.Replace(OVNState{
			LocalRouters:     []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1", LRPName: "lrp-r1", LRPMAC: lrpMAC}},
			HasLocalRouters:  true,
			NATIPToRouterMAC: map[string]string{fip: lrpMAC},
		})
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

		if !m.readiness.reconcileRan.Load() {
			t.Error("reconcileRan not set after a completed reconcile")
		}
		if m.readiness.lastReconcileOK.Load() {
			t.Error("lastReconcileOK true after a failed announce")
		}
	})

	// c. Healthy standby — modelled on
	// TestReconcileNoLocalRoutersInvokesRemoveAllRoutes. No local routers and
	// no VIPs takes the removeAllRoutes branch, which leaves cycleOK true.
	t.Run("healthy standby is ready", func(t *testing.T) {
		m := withTestMetrics(t)
		rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
		c, _, _ := newOVNClientWithFakes(t, "host-a")
		a := &Agent{
			cfg:            Config{},
			ovn:            c,
			routing:        rm,
			reconcileCh:    make(chan struct{}, 1),
			missingChassis: make(map[string]time.Time),
		}
		a.reconcile(context.Background(), "test")

		if !m.readiness.reconcileRan.Load() {
			t.Error("reconcileRan not set after a completed reconcile")
		}
		if !m.readiness.lastReconcileOK.Load() {
			t.Error("lastReconcileOK false for a healthy standby cycle")
		}
	})
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
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
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
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "198.51.100.20", "198.51.100.99"),
		nil,
	)

	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	a := &Agent{routing: rm, effectiveFilters: []*net.IPNet{cidr}}

	// Desired: 198.51.100.10 (new) and 198.51.100.20 (already in FRR).
	res := a.ensureRoutes([]string{"198.51.100.10", "198.51.100.20"}, []string{"198.51.100.10", "198.51.100.20"}, nil, nil)
	if !res.changed {
		t.Error("routeSyncResult.changed = false, want true (routes were added and removed)")
	}
	if !res.announced {
		t.Error("routeSyncResult.announced = false, want true (an FRR route was added)")
	}

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

// TestEnsureRoutesReportsReadyOutcome pins the route-sync outcome the drain
// takeover handshake keys on: ensureRoutes reports ready when the FIP routes
// were announced (or nothing needed changing) and not ready when the FRR add
// or the BGP soft-refresh failed. PortForwardOnly skips kernel routes so only
// the FRR/BGP paths — the ones the outcome depends on — are exercised, keeping
// the test off the real bridge.
func TestEnsureRoutesReportsReadyOutcome(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	newAgent := func(rec *vtyshRecorder) *Agent {
		rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
		return &Agent{cfg: Config{PortForwardOnly: true}, routing: rm, effectiveFilters: []*net.IPNet{cidr}}
	}

	t.Run("clean add is ready", func(t *testing.T) {
		rec := newVtyshRecorder()
		// FRR reports no routes, so the desired IP is added and BGP refreshed;
		// the recorder returns success for both.
		if !newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, nil, nil).ready {
			t.Errorf("clean add cycle: ready = false, want true (calls: %v)", rec.calls)
		}
	})

	t.Run("no-change cycle is ready", func(t *testing.T) {
		rec := newVtyshRecorder()
		// FRR already advertises the only desired IP: nothing is added or
		// removed, so no BGP refresh runs and the cycle is still ready.
		rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
			frrStaticRoutesJSON("169.254.0.1", "198.51.100.10"), nil)
		if !newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, nil, nil).ready {
			t.Errorf("no-change cycle: ready = false, want true (calls: %v)", rec.calls)
		}
	})

	t.Run("FRR add failure is not ready", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", "conf t", "-c", "vrf vrf-provider",
			"-c", "ip route 198.51.100.10/32 169.254.0.1", "-c", "exit-vrf", "-c", "end"},
			"", errors.New("vtysh add failed"))
		if newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, nil, nil).ready {
			t.Errorf("FRR add failure: ready = true, want false")
		}
	})

	t.Run("BGP refresh failure is not ready", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", "clear ip bgp vrf vrf-provider * soft out"},
			"", errors.New("bgp refresh failed"))
		if newAgent(rec).ensureRoutes([]string{"198.51.100.10"}, []string{"198.51.100.10"}, nil, nil).ready {
			t.Errorf("BGP refresh failure: ready = true, want false")
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
	const vip = "203.0.113.10"
	const jsonCmd = "show ip route vrf vrf-provider static json"
	newAgent := func(rec *vtyshRecorder) *Agent {
		rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
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

	// #223: the activity check runs on the FRR-static set, never on a VIP. A
	// pre-upgrade agent may have left an inactive static for the VIP in FRR, but
	// reconcile passes only frrStaticIPs (the OVN-derived set) here — the VIP is
	// announced through its connected route, so it is not in that set and can
	// never reach InactiveFRRRoutes to hold the gauge non-zero.
	t.Run("announceable VIP is not in the FRR set and never alarms", func(t *testing.T) {
		m := withTestMetrics(t)
		logs := captureSlog(t)

		rec := newVtyshRecorder()
		// FRR still carries an inactive static for the VIP; the OVN-derived FIP
		// the reconcile actually checks is healthy.
		rec.on([]string{"vtysh", "-c", jsonCmd},
			`{"`+vip+`/32":[{"prefix":"`+vip+`/32","protocol":"static"}],`+
				`"`+fip+`/32":[{"prefix":"`+fip+`/32","protocol":"static","selected":true,"installed":true}]}`, nil)
		// The set passed is frrStaticIPs — the FIP only, never the VIP.
		newAgent(rec).checkFRRRouteActivity([]string{fip})

		if got := inactiveRoutesGauge(t, m); got != 0 {
			t.Errorf("gauge = %v, want 0 — the VIP's static is not in the checked set", got)
		}
		if strings.Contains(logs.String(), "FRR static routes are configured but inactive") {
			t.Errorf("an announceable VIP must never raise the inactive-route alarm; logs:\n%s", logs.String())
		}
	})
}

// TestEnsureRoutesKeepsAnnounceSeparateFromRouteAdd verifies that a takeover
// reconcile with only additions issues the FRR route-add and the BGP
// soft-refresh as two separate vtysh invocations. Bundling them into one
// process makes the soft-refresh race the static→BGP redistribution, so the
// freshly added /32s miss the immediate re-advertise (issue #131 follow-up).
func TestEnsureRoutesKeepsAnnounceSeparateFromRouteAdd(t *testing.T) {
	rec := newVtyshRecorder()
	// FRR has no static routes yet — every desired route is a pure addition.
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		"",
		nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
	a := &Agent{routing: rm}

	res := a.ensureRoutes([]string{"198.51.100.10", "198.51.100.20"}, []string{"198.51.100.10", "198.51.100.20"}, nil, nil)
	if !res.announced || !res.changed {
		t.Fatalf("routeSyncResult = %+v, want changed+announced", res)
	}

	var addCall, refreshCall, bundled int
	for _, c := range rec.calls {
		joined := strings.Join(c, " ")
		hasAdd := strings.Contains(joined, "ip route 198.51.100.10/32") &&
			!strings.Contains(joined, "no ip route")
		hasRefresh := strings.Contains(joined, "clear ip bgp vrf vrf-provider")
		if hasAdd {
			addCall++
		}
		if hasRefresh {
			refreshCall++
		}
		if hasAdd && hasRefresh {
			bundled++
		}
	}
	if addCall != 1 || refreshCall != 1 {
		t.Errorf("expected one route-add and one BGP refresh call, got add=%d refresh=%d (calls: %v)", addCall, refreshCall, rec.calls)
	}
	if bundled != 0 {
		t.Errorf("route-add and BGP refresh must not share a vtysh call: %v", rec.calls)
	}
}

// TestRemoveAllRoutesWithStubbedFRRList exercises the FRR-driven removal
// path: a stub vtysh hook reports two managed routes, the agent batches
// the deletion, and a BGP soft-refresh follows because routes were removed.
func TestRemoveAllRoutesWithStubbedFRRList(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "198.51.100.10", "198.51.100.11"),
		nil,
	)
	rm := &RouteManager{
		// Use a synthetic bridge name that does not exist on either macOS
		// or Linux CI hosts so ListKernelRoutes errors out (netlink) or
		// returns "only supported on Linux" (stub) instead of touching real
		// kernel state. DryRun is intentionally false here because the
		// FRR-list short-circuits to nil in dry-run mode and would skip
		// the code path we want to exercise.
		cfg: Config{
			BridgeDev:   "ovnagent-nonexistent-br",
			VRFName:     "vrf-provider",
			VethNexthop: "169.254.0.1",
		},
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
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
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
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	// Populate state so reconcile takes the HasLocalRouters branch and
	// therefore reaches the ctx-aware EnsureGatewayRouting /
	// EnsureActivePriorityLead calls.
	c.state.Replace(OVNState{
		LocalRouters: []LocalRouterInfo{
			{
				RouterName:  "router1",
				RouterUUID:  "lr-1",
				LRPName:     "lrp-abc",
				LRPMAC:      "aa:aa:aa:aa:aa:aa",
				LRPNetworks: []string{"198.51.100.0/24"},
			},
		},
		HasLocalRouters:    true,
		DiscoveredNetworks: []*net.IPNet{cidr},
	})

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

// takeoverAgent builds an Agent whose OVN state has one locally-active
// router, so a reconcile cycle takes the HasLocalRouters branch and the
// announce adds the router's FIP /32 to FRR. The RouteManager is in
// dry-run, so the FRR/kernel helpers are no-ops.
func takeoverAgent(t *testing.T) (*Agent, *OVNClient) {
	t.Helper()
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.state.Replace(OVNState{
		LocalRouters: []LocalRouterInfo{{
			RouterName:  "router1",
			LRPName:     "lrp-abc",
			LRPMAC:      "aa:aa:aa:aa:aa:aa",
			LRPNetworks: []string{"198.51.100.0/24"},
		}},
		HasLocalRouters:    true,
		DiscoveredNetworks: []*net.IPNet{cidr},
	})
	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}
	return a, c
}

// observeFailoverAt stamps a chassisredirect observation made at `at`, then
// advances the published state generation the way the immediate refreshState
// pass that follows the observation does. The reconcile's snapshot therefore
// reflects the change and is entitled to consume the observation, which is the
// production sequence: stamp, refresh, announce.
func observeFailoverAt(t *testing.T, c *OVNClient, at time.Time) {
	t.Helper()
	c.failoverObserved.Store(&failoverObservation{at: at, gen: c.refreshSeq.Load()})
	st := c.state.Snapshot()
	st.Gen = c.refreshSeq.Add(1)
	c.state.Replace(st)
}

// failoverAnnounceSample returns the observation count and the summed
// observed seconds of the ovn_network_agent_failover_announce_seconds
// histogram. The sum matters as much as the count: the documented alert reads
// the observed interval, so a test that only counts samples would pass on a
// histogram recording the wrong duration.
func failoverAnnounceSample(t *testing.T, m *metricsRegistry) (uint64, float64) {
	t.Helper()
	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() == "ovn_network_agent_failover_announce_seconds" {
			h := mf.GetMetric()[0].GetHistogram()
			return h.GetSampleCount(), h.GetSampleSum()
		}
	}
	t.Fatal("failover_announce_seconds histogram missing from registry")
	return 0, 0
}

// TestReconcileRecordsFailoverAnnounceMetric verifies that a takeover
// reconcile triggered by a chassisredirect change records the
// failover-announce latency, and consumes the timestamp exactly once.
func TestReconcileRecordsFailoverAnnounceMetric(t *testing.T) {
	m := withTestMetrics(t)
	a, c := takeoverAgent(t)

	// A chassisredirect change was observed ~400ms ago.
	observeFailoverAt(t, c, time.Now().Add(-400*time.Millisecond))

	a.reconcile(context.Background(), triggerEvent)
	count, sum := failoverAnnounceSample(t, m)
	if count != 1 {
		t.Fatalf("failover_announce_seconds count after takeover = %d, want 1", count)
	}
	// The recorded value must be the observed→announce interval (~0.4s plus
	// the reconcile itself), not merely a sample: the p95 alert in
	// docs/guides/metrics.md reads the value, not the count. A guard on the
	// count alone would pass on a histogram recording 0.
	if sum < 0.4 || sum > 2.0 {
		t.Errorf("failover_announce_seconds sum = %v, want ~0.4 (the stamped interval)", sum)
	}

	// The timestamp is consumed: a follow-up reconcile with no new
	// observation must not record another sample.
	a.reconcile(context.Background(), triggerEvent)
	if count, _ := failoverAnnounceSample(t, m); count != 1 {
		t.Errorf("failover_announce_seconds count after second reconcile = %d, want 1", count)
	}
}

// TestReconcileLeavesFailoverStampForTheAnnouncingCycle covers the reconcile
// that loses the race to the takeover: a failover produces a storm of OVN
// changes, most of them not chassisredirect, so a debounce cycle is almost
// always in flight alongside the immediate path. That cycle refreshes state,
// arms its reconcile timer, and the chassisredirect change lands inside the
// window — so its reconcile runs on the pre-failover snapshot, announces
// nothing, and must leave the observation alone. Consuming it there loses the
// sample for the takeover reconcile that follows and does announce, so a
// genuine failover records nothing at all.
func TestReconcileLeavesFailoverStampForTheAnnouncingCycle(t *testing.T) {
	m := withTestMetrics(t)
	a, c := takeoverAgent(t)

	// The debounce cycle published the pre-failover snapshot (no local
	// routers), then the chassisredirect change was observed ~400ms ago.
	preFailover := c.state.Snapshot()
	preFailover.LocalRouters = nil
	preFailover.HasLocalRouters = false
	preFailover.Gen = c.refreshSeq.Add(1)
	c.state.Replace(preFailover)
	c.failoverObserved.Store(&failoverObservation{
		at:  time.Now().Add(-400 * time.Millisecond),
		gen: c.refreshSeq.Load(),
	})

	// The reconcile armed by the debounce cycle: it still holds the stale
	// snapshot, so it announces nothing and records nothing.
	a.reconcile(context.Background(), triggerEvent)
	if count, _ := failoverAnnounceSample(t, m); count != 0 {
		t.Fatalf("failover_announce_seconds count after the stale cycle = %d, want 0", count)
	}

	// The immediate refresh publishes the post-failover snapshot; the
	// reconcile it triggers is the one that announces the takeover routes and
	// must find the observation still pending.
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	postFailover := c.state.Snapshot()
	postFailover.LocalRouters = []LocalRouterInfo{{
		RouterName:  "router1",
		LRPName:     "lrp-abc",
		LRPMAC:      "aa:aa:aa:aa:aa:aa",
		LRPNetworks: []string{"198.51.100.0/24"},
	}}
	postFailover.HasLocalRouters = true
	postFailover.DiscoveredNetworks = []*net.IPNet{cidr}
	postFailover.Gen = c.refreshSeq.Add(1)
	c.state.Replace(postFailover)

	a.reconcile(context.Background(), triggerEvent)
	count, sum := failoverAnnounceSample(t, m)
	if count != 1 {
		t.Fatalf("failover_announce_seconds count after the takeover = %d, want 1 — "+
			"the stale cycle consumed the observation the announcing cycle needed", count)
	}
	// The sample must span from the observation, not from the takeover
	// reconcile: a stamp re-taken later would record a near-zero latency.
	if sum < 0.4 || sum > 2.0 {
		t.Errorf("failover_announce_seconds sum = %v, want ~0.4 (the observed→announce interval)", sum)
	}
}

// TestReconcileAnnouncesBeforeStabilitySteps pins the reconcile ordering
// introduced by issue #131: on a takeover the BGP announce — the FRR
// route-add followed by the "clear ip bgp ... soft out" soft-refresh — must
// run BEFORE the stability steps, and the post-change route verification must
// run AFTER it. Gating the announce behind the stability steps (priority lead,
// veth-leak, stale-chassis cleanup) adds seconds of external downtime on a
// failover, so a regression reverting the reorder must fail here.
//
// The prefix-list is deliberately NOT a stability step: it is the BGP
// outbound filter (neighbor ... prefix-list ... out) and permits the FIP
// /32s, so an announce that runs before it is populated advertises nothing.
// A standby chassis empties the list, so on a takeover the repopulation must
// precede the announce.
//
// The pin is expressed as an index ordering over the recorded vtysh calls:
//
//	prefix-list  "ip prefix-list fip-out ... permit 198.51.100.0/24 ge 32 le 32"
//	             (ReconcileFRRPrefixList — the BGP outbound filter)
//	  <
//	route-add    "ip route 198.51.100.0/32 ..."              (ensureRoutes)
//	  <
//	soft-refresh "clear ip bgp vrf vrf-provider * soft out"  (ensureRoutes)
//	  <
//	route re-add "ip route 198.51.100.0/32 ..."              (verifyRoutes)
//
// AC1 (the announce is not filtered away) is pinned by prefix-list <
// route-add < soft-refresh: with the prefix-list repopulation deferred to
// after ensureRoutes, the soft-refresh runs against an empty outbound filter
// and the takeover advertises zero prefixes, which inverts the assertion.
//
// AC2 (verification still runs, after the announce) is pinned by
// soft-refresh < route re-add. The vtysh recorder answers every FRR static
// listing with an empty document, so verifyRoutes finds the freshly announced
// /32 "missing" and re-adds it — a second route-add call that only
// verifyRoutes issues and that must land after the announce. The listing
// itself cannot anchor AC2: ListFRRRoutes and InactiveFRRRoutes share the
// same memoized "show ip route ... static json" query, so a second listing
// appears with or without verifyRoutes (checkFRRRouteActivity re-reads too).
//
// EnsureActivePriorityLead and the stale-chassis cleanup are NB/SB-side, not
// vtysh, so they cannot be interleaved with the vtysh stream and are not
// asserted here.
func TestReconcileAnnouncesBeforeStabilitySteps(t *testing.T) {
	rec := newVtyshRecorder()
	// Non-dry-run RouteManager so the FRR helpers actually reach the vtysh
	// hook (dry-run would short-circuit them). FRRPrefixList is non-empty so
	// ReconcileFRRPrefixList populates the outbound filter. The OVS hook is a
	// no-op recorder so the pre-announce OVS steps (EnsureSegments, hairpin
	// flows) don't shell out on this host, and the kernel-route listing hook
	// keeps verifyRoutes from bailing out before its FRR re-read on hosts
	// where kernel routes are unsupported.
	rm := &RouteManager{
		cfg:                  Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", FRRPrefixList: "fip-out"},
		execVtyshHook:        rec.hook(),
		execOVSHook:          newOVSRecorder().hook(),
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) { return nil, nil },
	}

	// Same OVN rig as takeoverAgent: one locally-active router whose LRP
	// network 198.51.100.0/24 makes 198.51.100.0 the desired announce IP.
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.state.Replace(OVNState{
		LocalRouters: []LocalRouterInfo{{
			RouterName:  "router1",
			LRPName:     "lrp-abc",
			LRPMAC:      "aa:aa:aa:aa:aa:aa",
			LRPNetworks: []string{"198.51.100.0/24"},
		}},
		HasLocalRouters:    true,
		DiscoveredNetworks: []*net.IPNet{cidr},
	})
	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	// The kernel-route add helpers error out on this host (no br-ex);
	// reconcile only logs those and proceeds to the FRR/vtysh path, which is
	// all this test inspects.
	a.reconcile(context.Background(), "event")

	// firstIdx locates a recorded vtysh call by a substring of its full,
	// space-joined args; -1 when absent. allIdxs returns every match.
	firstIdx := func(sub string) int {
		for i, args := range rec.calls {
			if strings.Contains(strings.Join(args, " "), sub) {
				return i
			}
		}
		return -1
	}
	allIdxs := func(sub string) []int {
		var idxs []int
		for i, args := range rec.calls {
			if strings.Contains(strings.Join(args, " "), sub) {
				idxs = append(idxs, i)
			}
		}
		return idxs
	}
	dump := func() string {
		var b strings.Builder
		for i, args := range rec.calls {
			fmt.Fprintf(&b, "\n  [%d] %s", i, strings.Join(args, " "))
		}
		return b.String()
	}

	// route-add occurrences: the announce's add of the desired /32
	// (ensureRoutes), then verifyRoutes' re-add of the same /32.
	routeAdds := allIdxs("ip route 198.51.100.0/32")
	softRefresh := firstIdx("clear ip bgp vrf vrf-provider * soft out")
	// The prefix-list populate: the entry that permits the FIP /32s through
	// the BGP outbound filter.
	prefixListAdd := firstIdx("permit 198.51.100.0/24 ge 32 le 32")

	if len(routeAdds) == 0 || softRefresh < 0 || prefixListAdd < 0 {
		t.Fatalf("missing expected vtysh calls: prefixListAdd=%d routeAdds=%v softRefresh=%d; calls:%s",
			prefixListAdd, routeAdds, softRefresh, dump())
	}

	// AC1: the outbound filter is populated before the announce, and the
	// announce is route-add followed by soft-refresh. Deferring the
	// prefix-list to after the announce advertises nothing.
	if prefixListAdd >= routeAdds[0] || routeAdds[0] >= softRefresh {
		t.Errorf("prefix-list must be populated before the announce: prefixListAdd=%d routeAdd=%d softRefresh=%d; calls:%s",
			prefixListAdd, routeAdds[0], softRefresh, dump())
	}

	// AC2: verifyRoutes still runs, after the announce. The recorder reports
	// an empty FRR document on every listing, so verifyRoutes re-adds the
	// /32 it just announced — deleting verifyRoutes drops this second add.
	if len(routeAdds) < 2 {
		t.Fatalf("want the announce's route-add plus verifyRoutes' re-add, got %v; calls:%s",
			routeAdds, dump())
	}
	if reAdd := routeAdds[1]; reAdd <= softRefresh {
		t.Errorf("route verification must run after the announce: softRefresh=%d reAdd=%d; calls:%s",
			softRefresh, reAdd, dump())
	}
}

// TestReconcileSkipsFailoverMetricOnStartup verifies that the startup
// reconcile never records a failover-announce sample, even though it adds
// FRR routes — startup is not a failover.
func TestReconcileSkipsFailoverMetricOnStartup(t *testing.T) {
	m := withTestMetrics(t)
	a, c := takeoverAgent(t)
	observeFailoverAt(t, c, time.Now())

	a.reconcile(context.Background(), triggerStartup)
	if count, _ := failoverAnnounceSample(t, m); count != 0 {
		t.Errorf("failover_announce_seconds count after startup = %d, want 0", count)
	}
	// The startup cycle still consumes the timestamp so it cannot leak.
	if !c.takeFailoverObservedAt(c.GetState().Gen).IsZero() {
		t.Error("startup reconcile must consume the failover timestamp")
	}
}

// TestEnsureRoutesRemovalOnlyIsNotAnnounce verifies that a cycle which only
// withdraws routes reports changed (so verification still runs) but not
// announced: withdrawing a /32 attracts no traffic, so it is not a takeover
// announce and must not feed the failover-latency metric.
// TestReconcilePrefixListCarriesVIPOutsideHostedNetworks pins #226: a
// port-forward VIP outside every hosted network must still get its own exact
// "permit <vip>/32" prefix-list entry. The VIP announces through its connected
// route filtered by that list; before the fix the list held only the hosted
// networks' CIDRs, so a flat-range VIP on a chassis hosting only VLAN routers
// passed the announceability gate (HasLocalRouters), had its address and DNAT
// installed — and was silently never exported. The nightly heterogeneous chaos
// profile blackholed its API VIP exactly this way the moment a priority flip
// moved the flat router elsewhere.
func TestReconcilePrefixListCarriesVIPOutsideHostedNetworks(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{
		cfg:                  Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", FRRPrefixList: "fip-out"},
		execVtyshHook:        rec.hook(),
		execOVSHook:          newOVSRecorder().hook(),
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) { return nil, nil },
	}

	// One locally-active VLAN router; the VIP 192.0.2.80 lies outside its
	// 198.51.100.0/24 network — the heterogeneous split.
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.state.Replace(OVNState{
		LocalRouters: []LocalRouterInfo{{
			RouterName:  "router-vlan101",
			LRPName:     "lrp-vlan101",
			LRPMAC:      "aa:aa:aa:aa:aa:aa",
			LRPNetworks: []string{"198.51.100.0/24"},
		}},
		HasLocalRouters:    true,
		DiscoveredNetworks: []*net.IPNet{cidr},
	})
	a := &Agent{
		cfg:            Config{PortForwards: []PortForwardVIP{{VIP: "192.0.2.80", ManageVIP: true}}},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	a.reconcile(context.Background(), "test")

	var sawVIP, sawNetwork bool
	for _, args := range rec.calls {
		j := strings.Join(args, " ")
		if strings.Contains(j, "ip prefix-list fip-out seq") &&
			strings.HasSuffix(j, "permit 192.0.2.80/32 -c end") &&
			!strings.Contains(j, "no ip prefix-list") {
			sawVIP = true
		}
		if strings.Contains(j, "permit 198.51.100.0/24 ge 32 le 32") {
			sawNetwork = true
		}
	}
	if !sawVIP {
		t.Errorf("reconcile must add an exact permit for the VIP /32 even though no hosted network covers it; calls: %v", rec.calls)
	}
	if !sawNetwork {
		t.Errorf("the hosted network's ge/le entry must still be added; calls: %v", rec.calls)
	}
}

// TestReconcileLeakPlaneClaimsOnlyOwnedNetworks pins #258: with a manual
// network_cidr list, the veth-leak plane must claim only the networks whose
// routers this chassis actually holds. Before the fix the reconcile handed the
// leak plane the raw filter list, so a gateway holding only a VLAN router
// programmed a leak route for the flat provider network too — and that route,
// more specific than the provider VRF's default (#247), steered split-owner
// port-forward replies into the local OVN where nothing answers.
func TestReconcileLeakPlaneClaimsOnlyOwnedNetworks(t *testing.T) {
	var manual []*net.IPNet
	for _, s := range []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"} {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		manual = append(manual, n)
	}

	var leakCalls [][]*net.IPNet
	rm := &RouteManager{
		cfg:                  Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", NetworkFilters: manual},
		execVtyshHook:        newVtyshRecorder().hook(),
		execOVSHook:          newOVSRecorder().hook(),
		listKernelRoutesHook: func() ([]kernelRouteEntry, error) { return nil, nil },
		reconcileVethLeakHook: func(desired []*net.IPNet) error {
			leakCalls = append(leakCalls, desired)
			return nil
		},
	}

	// The chassis holds only the VLAN router; the flat network's
	// chassisredirect port lives on another gateway.
	_, owned, _ := net.ParseCIDR("198.51.100.0/24")
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.state.Replace(OVNState{
		LocalRouters: []LocalRouterInfo{{
			RouterName:  "router-vlan101",
			LRPName:     "lrp-vlan101",
			LRPMAC:      "aa:aa:aa:aa:aa:aa",
			LRPNetworks: []string{"198.51.100.0/24"},
		}},
		HasLocalRouters:    true,
		DiscoveredNetworks: []*net.IPNet{owned},
	})
	a := &Agent{
		cfg:            rm.cfg,
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	a.reconcile(context.Background(), "test")

	if len(leakCalls) == 0 {
		t.Fatal("reconcile never reached the veth-leak plane")
	}
	got := netStrings(leakCalls[len(leakCalls)-1])
	want := []string{"198.51.100.0/24"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("leak plane got %v, want only the owned network %v", got, want)
	}
}

func TestEnsureRoutesRemovalOnlyIsNotAnnounce(t *testing.T) {
	rec := newVtyshRecorder()
	// FRR still carries a /32 that is no longer desired.
	rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "203.0.113.5"), nil)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
	a := &Agent{
		cfg:            Config{},
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	// Disjoint desired set: the stale /32 is withdrawn, nothing is added.
	res := a.ensureRoutes(nil, nil, nil, nil)
	if !res.changed {
		t.Errorf("changed = false, want true — the stale /32 was withdrawn")
	}
	if res.announced {
		t.Errorf("announced = true, want false — a withdrawal is not an announce")
	}
}

// TestEnsureRoutesAnnounceReportsOutcomeNotIntent verifies that an announce
// which failed is not reported as one that happened. The FRR route-add and the
// BGP soft-refresh both only log their error and let the cycle continue, so
// deriving announced from the intent to run them — the list of routes to add —
// claims a successful announce whenever either silently failed.
//
// The failure is correlated with the event under measurement: the takeover
// chassis is under stress precisely because of whatever moved the gateway to
// it, and an unavailable or restarting vtysh fails both calls. The histogram
// would then record a fast, healthy failover and keep the p95 alert green
// while zero FIPs are advertised and every FIP on the router is unreachable
// from outside — the alert is worth less than none at all.
func TestEnsureRoutesAnnounceReportsOutcomeNotIntent(t *testing.T) {
	tests := []struct {
		name   string
		failOn string // substring of the vtysh args that must fail
	}{
		{"route-add fails", "ip route 198.51.100.7/32"},
		{"soft-refresh fails", "clear ip bgp"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rm := &RouteManager{
				cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"},
				execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
					if strings.Contains(strings.Join(cmd.Args, " "), tc.failOn) {
						return []byte("vtysh: failed to connect to zebra"), errors.New("exit status 1")
					}
					return nil, nil
				},
			}
			// Port-forward-only keeps ensureRoutes off the kernel-route path,
			// so the cycle under test is exactly the FRR add plus the
			// soft-refresh on every platform. The empty route listing above
			// makes the desired /32 missing, so the add is attempted.
			a := &Agent{
				cfg:            Config{PortForwardOnly: true},
				routing:        rm,
				reconcileCh:    make(chan struct{}, 1),
				missingChassis: make(map[string]time.Time),
			}

			res := a.ensureRoutes([]string{"198.51.100.7"}, []string{"198.51.100.7"}, nil, nil)
			if !res.changed {
				t.Error("changed = false, want true — the cycle still touched FRR, so verification must run")
			}
			if res.announced {
				t.Error("announced = true after the announce failed: the failover histogram " +
					"would record a fast, healthy takeover while nothing is advertised")
			}
		})
	}
}

// TestReconcileSkipsFailoverMetricWithoutAnnounce verifies that a reconcile
// which changes routes but announces nothing records no failover sample, even
// with a chassisredirect observation pending. Only an announce ends a
// failover, so timing a withdrawal-only cycle would report a bogus latency.
func TestReconcileSkipsFailoverMetricWithoutAnnounce(t *testing.T) {
	m := withTestMetrics(t)

	rec := newVtyshRecorder()
	// The desired /32 (198.51.100.0, from the router's LRP network) is
	// already in FRR, so nothing is added and the cycle cannot announce.
	// 198.51.100.77 falls inside the discovered /24 — it is managed, no
	// longer desired, and therefore withdrawn: changed, but not announced.
	rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		frrStaticRoutesJSON("169.254.0.1", "198.51.100.0", "198.51.100.77"), nil)
	rm := &RouteManager{
		cfg:           Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"},
		execVtyshHook: rec.hook(),
		execOVSHook:   newOVSRecorder().hook(),
	}

	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.state.Replace(OVNState{
		LocalRouters: []LocalRouterInfo{{
			RouterName:  "router1",
			LRPName:     "lrp-abc",
			LRPMAC:      "aa:aa:aa:aa:aa:aa",
			LRPNetworks: []string{"198.51.100.0/24"},
		}},
		HasLocalRouters:    true,
		DiscoveredNetworks: []*net.IPNet{cidr},
	})
	a := &Agent{
		cfg:            Config{},
		ovn:            c,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		missingChassis: make(map[string]time.Time),
	}

	observeFailoverAt(t, c, time.Now().Add(-400*time.Millisecond))

	a.reconcile(context.Background(), triggerEvent)
	if count, _ := failoverAnnounceSample(t, m); count != 0 {
		t.Errorf("failover_announce_seconds count after a withdrawal-only cycle = %d, want 0", count)
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

// TestDrainOutcome pins the drain_total outcome precedence: a drain error
// outranks a timeout, a timeout outranks a noop, and only an unqualified
// success — something drained, no error, no deadline — reports "completed".
func TestDrainOutcome(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name    string
		err     error
		ctxErr  error
		drained bool
		want    string
	}{
		{"error outranks timeout, noop and a drained result", errBoom, context.DeadlineExceeded, false, "error"},
		{"error outranks a clean drained result", errBoom, nil, true, "error"},
		{"timeout outranks noop when nothing drained", nil, context.DeadlineExceeded, false, "timeout"},
		{"timeout outranks a drained result", nil, context.DeadlineExceeded, true, "timeout"},
		{"nothing drained is noop", nil, nil, false, "noop"},
		{"drained with no error or deadline is completed", nil, nil, true, "completed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := drainOutcome(tt.err, tt.ctxErr, tt.drained); got != tt.want {
				t.Errorf("drainOutcome(%v, %v, %v) = %q, want %q", tt.err, tt.ctxErr, tt.drained, got, tt.want)
			}
		})
	}
}
