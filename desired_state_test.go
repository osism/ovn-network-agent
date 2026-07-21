package main

import (
	"reflect"
	"testing"
)

func vlan(t int) *int { return &t }

// TestBuildHairpinTargets covers the two sources of hairpin targets — the NAT
// external IPs and the router gateway IPs — plus the rejection paths: an LRP
// with no MAC contributes nothing (a flow cannot set a dl_dst it does not
// know), and a malformed LRP network is skipped rather than poisoning the map.
func TestBuildHairpinTargets(t *testing.T) {
	state := OVNState{
		NATIPToRouterMAC: map[string]string{
			"198.51.100.50": "aa:aa:aa:aa:aa:01",
			"2001:db8::50":  "aa:aa:aa:aa:aa:01", // dual-stack: kept here
		},
		NATIPToSegment: map[string]string{
			"198.51.100.50": "physnet1-port",
			// 2001:db8::50 deliberately absent — unresolved segment.
		},
		LocalRouters: []LocalRouterInfo{
			{
				RouterName: "r1", LRPName: "lrp-1", LRPMAC: "aa:aa:aa:aa:aa:01",
				LRPNetworks: []string{"198.51.100.1/24", "not-a-cidr"},
				Segment:     &LocalnetSegment{LocalnetPort: "physnet1-port"},
			},
			{
				// No MAC: its gateway IP must NOT become a hairpin target.
				RouterName: "r2", LRPName: "lrp-2", LRPMAC: "",
				LRPNetworks: []string{"203.0.113.1/24"},
			},
		},
	}

	got := buildHairpinTargets(state)

	want := map[string]HairpinTarget{
		"198.51.100.50": {RouterMAC: "aa:aa:aa:aa:aa:01", Segment: "physnet1-port"},
		"2001:db8::50":  {RouterMAC: "aa:aa:aa:aa:aa:01", Segment: ""},
		"198.51.100.1":  {RouterMAC: "aa:aa:aa:aa:aa:01", Segment: "physnet1-port"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildHairpinTargets() = %#v, want %#v", got, want)
	}
	if _, ok := got["203.0.113.1"]; ok {
		t.Error("a MAC-less LRP's gateway IP must not become a hairpin target")
	}
	if _, ok := got["not-a-cidr"]; ok {
		t.Error("a malformed LRP network must be skipped")
	}
}

// TestBuildDesiredSegments pins the dedup, the "" fallback for routers whose
// segment is unresolved, and the sort order the data plane relies on.
func TestBuildDesiredSegments(t *testing.T) {
	segments, byName := buildDesiredSegments([]LocalRouterInfo{
		// Two routers on the same segment — must collapse to one entry.
		{LRPName: "lrp-1", Segment: &LocalnetSegment{LocalnetPort: "physnet2-port", VLANTag: vlan(102)}},
		{LRPName: "lrp-2", Segment: &LocalnetSegment{LocalnetPort: "physnet2-port", VLANTag: vlan(102)}},
		{LRPName: "lrp-3", Segment: &LocalnetSegment{LocalnetPort: "physnet1-port", VLANTag: vlan(101)}},
		// Unresolved segment → the "" fallback.
		{LRPName: "lrp-4", Segment: nil},
	})

	want := []DesiredSegment{
		{LocalnetPort: "", VLANTag: nil}, // "" sorts first
		{LocalnetPort: "physnet1-port", VLANTag: vlan(101)},
		{LocalnetPort: "physnet2-port", VLANTag: vlan(102)},
	}
	if len(segments) != len(want) {
		t.Fatalf("buildDesiredSegments() returned %d segments, want %d: %+v", len(segments), len(want), segments)
	}
	for i := range want {
		if segments[i].LocalnetPort != want[i].LocalnetPort {
			t.Errorf("segments[%d].LocalnetPort = %q, want %q", i, segments[i].LocalnetPort, want[i].LocalnetPort)
		}
		switch {
		case want[i].VLANTag == nil && segments[i].VLANTag != nil:
			t.Errorf("segments[%d].VLANTag = %v, want nil (flat)", i, *segments[i].VLANTag)
		case want[i].VLANTag != nil && segments[i].VLANTag == nil:
			t.Errorf("segments[%d].VLANTag = nil, want %d", i, *want[i].VLANTag)
		case want[i].VLANTag != nil && *segments[i].VLANTag != *want[i].VLANTag:
			t.Errorf("segments[%d].VLANTag = %d, want %d", i, *segments[i].VLANTag, *want[i].VLANTag)
		}
	}

	// The index must expose exactly the same set, including the "" fallback.
	if len(byName) != len(want) {
		t.Errorf("SegmentByName has %d entries, want %d: %+v", len(byName), len(want), byName)
	}
	if _, ok := byName[""]; !ok {
		t.Error("SegmentByName must contain the \"\" fallback segment")
	}
}

func TestBuildDesiredSegmentsNoRouters(t *testing.T) {
	segments, byName := buildDesiredSegments(nil)
	if len(segments) != 0 {
		t.Errorf("segments = %+v, want empty for no local routers", segments)
	}
	if len(byName) != 0 {
		t.Errorf("SegmentByName = %+v, want empty for no local routers", byName)
	}
}

// TestSplitIPv4 pins the fork between the dual-stack hairpin plane and the
// IPv4-only route/announce plane. A v6 address (or an unparseable key) must be
// reported as excluded, never announced: it would fail the FRR batch and block
// the takeover marker.
func TestSplitIPv4(t *testing.T) {
	v4, nonV4 := splitIPv4(map[string]HairpinTarget{
		"198.51.100.50": {},
		"198.51.100.10": {},
		"2001:db8::50":  {},
		"2001:db8::10":  {},
		"garbage":       {},
	})

	if want := []string{"198.51.100.10", "198.51.100.50"}; !reflect.DeepEqual(v4, want) {
		t.Errorf("v4 = %v, want %v (sorted)", v4, want)
	}
	// Unparseable keys are excluded alongside real v6 addresses — they must
	// never reach a route/FRR command payload.
	if want := []string{"2001:db8::10", "2001:db8::50", "garbage"}; !reflect.DeepEqual(nonV4, want) {
		t.Errorf("nonV4 = %v, want %v (sorted)", nonV4, want)
	}
}

func TestSplitIPv4Empty(t *testing.T) {
	v4, nonV4 := splitIPv4(nil)
	if len(v4) != 0 || len(nonV4) != 0 {
		t.Errorf("splitIPv4(nil) = (%v, %v), want empty", v4, nonV4)
	}
}

// TestComputeDesiredState drives the whole derivation, including the VIP merge:
// announceable port-forward VIPs join the desired set, and a VIP that duplicates
// a FIP must appear once.
func TestComputeDesiredState(t *testing.T) {
	state := OVNState{
		NATIPToRouterMAC: map[string]string{
			"198.51.100.50": "aa:aa:aa:aa:aa:01",
			"2001:db8::50":  "aa:aa:aa:aa:aa:01",
		},
		LocalRouters: []LocalRouterInfo{
			{LRPName: "lrp-1", LRPMAC: "aa:aa:aa:aa:aa:01",
				Segment: &LocalnetSegment{LocalnetPort: "physnet1-port", VLANTag: vlan(101)}},
		},
	}
	forwards := []PortForwardVIP{
		{VIP: "203.0.113.10"},
		// Duplicates an existing FIP — must be deduplicated, not doubled.
		{VIP: "198.51.100.50"},
	}

	got := computeDesiredState(state, forwards, true)

	if len(got.DormantVIPs) != 0 {
		t.Errorf("DormantVIPs = %v, want empty when the VIPs are announceable", got.DormantVIPs)
	}
	if want := []string{"198.51.100.50", "203.0.113.10"}; !reflect.DeepEqual(got.DesiredIPs, want) {
		t.Errorf("DesiredIPs = %v, want %v (VIPs merged, deduped, sorted)", got.DesiredIPs, want)
	}
	if want := []string{"198.51.100.50"}; !reflect.DeepEqual(got.HairpinIPs, want) {
		t.Errorf("HairpinIPs = %v, want %v (VIPs excluded, v6 excluded)", got.HairpinIPs, want)
	}
	// #223: the VIP is in DesiredIPs (kernel plane) but never in FRRStaticIPs —
	// it announces through its connected route, not an FRR static. FRRStaticIPs
	// equals HairpinIPs (the OVN-derived set).
	if want := []string{"198.51.100.50"}; !reflect.DeepEqual(got.FRRStaticIPs, want) {
		t.Errorf("FRRStaticIPs = %v, want %v (VIPs excluded, equal to HairpinIPs)", got.FRRStaticIPs, want)
	}
	for _, vip := range []string{"203.0.113.10"} {
		for _, ip := range got.FRRStaticIPs {
			if ip == vip {
				t.Errorf("an announceable VIP %s must not appear in FRRStaticIPs %v", vip, got.FRRStaticIPs)
			}
		}
	}
	if want := []string{"2001:db8::50"}; !reflect.DeepEqual(got.ExcludedNonV4, want) {
		t.Errorf("ExcludedNonV4 = %v, want %v", got.ExcludedNonV4, want)
	}
	// The v6 FIP stays in the dual-stack hairpin plane.
	if _, ok := got.HairpinTargets["2001:db8::50"]; !ok {
		t.Error("the v6 FIP must remain a hairpin target")
	}
	if len(got.Segments) != 1 || got.Segments[0].LocalnetPort != "physnet1-port" {
		t.Errorf("Segments = %+v, want the single physnet1-port segment", got.Segments)
	}

	// With no port forwards at all, FRRStaticIPs equals HairpinIPs — there is no
	// VIP to exclude, and the OVN-derived set is exactly what gets a static.
	noForwards := computeDesiredState(state, nil, true)
	if !reflect.DeepEqual(noForwards.FRRStaticIPs, noForwards.HairpinIPs) {
		t.Errorf("FRRStaticIPs = %v, want it to equal HairpinIPs %v when portForwards is empty",
			noForwards.FRRStaticIPs, noForwards.HairpinIPs)
	}
}

// TestComputeDesiredStatePortForwardOnly covers the port-forward-only mode,
// where reconcile passes the zero OVNState: the derivation must reduce to the
// VIPs alone without touching any nil map.
func TestComputeDesiredStatePortForwardOnly(t *testing.T) {
	got := computeDesiredState(OVNState{}, []PortForwardVIP{{VIP: "203.0.113.10"}}, true)

	if want := []string{"203.0.113.10"}; !reflect.DeepEqual(got.DesiredIPs, want) {
		t.Errorf("DesiredIPs = %v, want %v", got.DesiredIPs, want)
	}
	// #223: in port-forward-only mode the zero OVNState yields no HairpinIPs, so
	// FRRStaticIPs is empty — the VIP announces through its connected route.
	if len(got.FRRStaticIPs) != 0 {
		t.Errorf("FRRStaticIPs = %v, want empty in port-forward-only mode", got.FRRStaticIPs)
	}
	if len(got.HairpinTargets) != 0 {
		t.Errorf("HairpinTargets = %v, want empty with no OVN state", got.HairpinTargets)
	}
	if len(got.Segments) != 0 {
		t.Errorf("Segments = %+v, want empty with no OVN state", got.Segments)
	}
}

// TestComputeDesiredStateDormantVIPs covers the routerless gateway of #206: with
// the VIPs not announceable they must leave the route plane entirely — no kernel
// route, no FRR static route — and be reported as dormant instead. Installing
// them anyway left an unadvertisable FRR route that alarmed on every cycle.
func TestComputeDesiredStateDormantVIPs(t *testing.T) {
	forwards := []PortForwardVIP{
		{VIP: "203.0.113.10"},
		{VIP: "192.0.2.99"},
		// A duplicate must not be reported twice.
		{VIP: "203.0.113.10"},
	}

	got := computeDesiredState(OVNState{}, forwards, false)

	if len(got.DesiredIPs) != 0 {
		t.Errorf("DesiredIPs = %v, want empty — dormant VIPs get no routes", got.DesiredIPs)
	}
	// A dormant VIP is never an FRR static either — it is withheld by address,
	// not by route (#223), and FRRStaticIPs is the OVN-derived set, empty here.
	if len(got.FRRStaticIPs) != 0 {
		t.Errorf("FRRStaticIPs = %v, want empty — a dormant VIP gets no static", got.FRRStaticIPs)
	}
	if want := []string{"192.0.2.99", "203.0.113.10"}; !reflect.DeepEqual(got.DormantVIPs, want) {
		t.Errorf("DormantVIPs = %v, want %v (deduped, sorted)", got.DormantVIPs, want)
	}
}

// TestComputeDesiredStateDormantVIPsKeepFIPs guards the blast radius: dormancy
// applies to the VIPs alone. A node that somehow holds FIPs must still announce
// them, so only the VIP entries are withheld from DesiredIPs.
func TestComputeDesiredStateDormantVIPsKeepFIPs(t *testing.T) {
	state := OVNState{
		NATIPToRouterMAC: map[string]string{"198.51.100.50": "aa:aa:aa:aa:aa:01"},
	}

	got := computeDesiredState(state, []PortForwardVIP{{VIP: "203.0.113.10"}}, false)

	if want := []string{"198.51.100.50"}; !reflect.DeepEqual(got.DesiredIPs, want) {
		t.Errorf("DesiredIPs = %v, want %v — the FIP stays, only the VIP is dormant", got.DesiredIPs, want)
	}
	// The FIP keeps its FRR static; the dormant VIP was never one. FRRStaticIPs
	// equals HairpinIPs (the FIP alone).
	if want := []string{"198.51.100.50"}; !reflect.DeepEqual(got.FRRStaticIPs, want) {
		t.Errorf("FRRStaticIPs = %v, want %v — the FIP static stays, the VIP is not one", got.FRRStaticIPs, want)
	}
	if want := []string{"203.0.113.10"}; !reflect.DeepEqual(got.DormantVIPs, want) {
		t.Errorf("DormantVIPs = %v, want %v", got.DormantVIPs, want)
	}
}
