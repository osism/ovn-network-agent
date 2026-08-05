package main

import (
	"testing"
)

// The oracle follows the chassisredirect owner: the gateway that owns a
// router's CR port carries that router's FIP and gateway-IP routes on the right
// per-segment kernel device, while a standby gateway carries nothing at all.
func TestExpectationFollowsTheChassisredirectOwner(t *testing.T) {
	// gateway-1 owns the flat router and a VLAN-101 router; gateway-2 owns
	// nothing, so it is a standby.
	snap := ovnSnapshot{
		CRPortChassis: map[string]string{
			"cr-lr0-public":        "gateway-1",
			"cr-lr-vlan101-public": "gateway-1",
		},
		LRPs: map[string]lrpRow{
			"lr0-public":        {Networks: []string{"192.0.2.1/24"}, GatewayChassis: []string{"gc-lr0-1"}},
			"lr-vlan101-public": {Networks: []string{"198.51.100.1/24"}, GatewayChassis: []string{"gc-v101-1"}},
		},
		LRPNameByUUID: map[string]string{
			"uuid-lr0-public":        "lr0-public",
			"uuid-lr-vlan101-public": "lr-vlan101-public",
		},
		Routers: map[string]routerRow{
			"lr0": {
				LRPUUIDs: []string{"uuid-lr0-public"},
				NATs:     []natRow{{ExternalIP: "192.0.2.10", Type: "dnat_and_snat"}},
			},
			"lr-vlan101": {
				LRPUUIDs: []string{"uuid-lr-vlan101-public"},
				NATs:     []natRow{{ExternalIP: "198.51.100.10", Type: "dnat_and_snat"}},
			},
		},
		GatewayChassis: map[string]gatewayChassisRow{
			"gc-lr0-1":  {Name: "lr0-public-gateway-1", ChassisName: "gateway-1", Priority: 30},
			"gc-v101-1": {Name: "lr-vlan101-public-gateway-1", ChassisName: "gateway-1", Priority: 30},
		},
		StaticRoutes: []staticRouteRow{
			{UUID: "sr-1", IPPrefix: "192.0.2.10/32", ExternalIDs: map[string]string{"ovn-network-agent": "managed"}},
		},
		Chassis:         map[string]bool{"gateway-1": true, "gateway-2": true, "gateway-3": true},
		SegmentTagByLRP: map[string]int{"lr0-public": 0, "lr-vlan101-public": 101},
	}
	doc := fullModeDoc(t)

	t.Run("active owner", func(t *testing.T) {
		exp := computeExpectation(snap, "gateway-1", doc, "172.20.20.4")

		if exp.Mode != "full" || !exp.Active {
			t.Fatalf("mode=%q active=%v, want a full-mode active gateway", exp.Mode, exp.Active)
		}
		wantIPs := []string{"192.0.2.1", "192.0.2.10", "198.51.100.1", "198.51.100.10"}
		assertSet(t, "DesiredIPs", exp.DesiredIPs, wantIPs)
		assertSet(t, "FRRStatic", exp.FRRStatic, wantIPs)
		assertSet(t, "Hairpin", exp.Hairpin, wantIPs)
		// The flat FIP/gateway IPs route on br-ex; the VLAN-101 ones route on
		// the segment subinterface br-ex.101.
		assertDevs(t, exp.KernelRouteDev, map[string]string{
			"192.0.2.1":     "br-ex",
			"192.0.2.10":    "br-ex",
			"198.51.100.1":  "br-ex.101",
			"198.51.100.10": "br-ex.101",
		})
		if exp.SkipKernel {
			t.Fatal("kernel plane must not be skipped on a full-mode gateway")
		}
		assertSet(t, "PrefixList", exp.PrefixList, []string{"192.0.2.0/24", "198.51.100.0/24"})
		// Two segments (flat + VLAN-101), two MAC-tweak flows each.
		if exp.MACTweakFlows != 4 {
			t.Fatalf("MACTweakFlows = %d, want 4 (two per segment, two segments)", exp.MACTweakFlows)
		}
		assertSet(t, "MustAnnounce", exp.MustAnnounce, wantIPs)
		assertSet(t, "AnnounceBound", exp.AnnounceBound, wantIPs)
	})

	t.Run("standby owner", func(t *testing.T) {
		exp := computeExpectation(snap, "gateway-2", doc, "172.20.20.5")

		if exp.Active {
			t.Fatal("gateway-2 owns no CR port, it must not be active")
		}
		if len(exp.DesiredIPs) != 0 {
			t.Fatalf("DesiredIPs = %v, want none on a standby gateway", exp.DesiredIPs)
		}
		if len(exp.FRRStatic) != 0 {
			t.Fatalf("FRRStatic = %v, want no FRR routes on a standby gateway", exp.FRRStatic)
		}
		if len(exp.KernelRouteDev) != 0 {
			t.Fatalf("KernelRouteDev = %v, want no kernel routes on a standby gateway", exp.KernelRouteDev)
		}
		// The agent removes the hairpin flows and cleans the prefix-list on a
		// standby node.
		if exp.Hairpin != nil {
			t.Fatalf("Hairpin = %v, want the empty set on a standby gateway", exp.Hairpin)
		}
		if exp.SkipHairpin {
			t.Fatal("the hairpin plane is verified (not skipped) in full mode")
		}
		if exp.PrefixList != nil {
			t.Fatalf("PrefixList = %v, want it absent (nil) on a standby gateway", exp.PrefixList)
		}
		// The agent does not clean MAC-tweak flows on standby, so the count is
		// unknowable and the check is skipped.
		if exp.MACTweakFlows != -1 {
			t.Fatalf("MACTweakFlows = %d, want -1 (skip) on a standby gateway", exp.MACTweakFlows)
		}
		if len(exp.MustAnnounce) != 0 {
			t.Fatalf("MustAnnounce = %v, want nothing required on a standby gateway", exp.MustAnnounce)
		}
	})
}

// A NAT IP outside the network_cidr filter must fall out of every plane, while
// the router's own gateway IPs stay desired regardless of the filter — and the
// announced presence set drops the desired IPs the filter does not cover.
func TestExpectationExcludesFIPsOutsideTheNetworkCIDRFilter(t *testing.T) {
	snap := ovnSnapshot{
		CRPortChassis: map[string]string{"cr-lr0-public": "gateway-1"},
		LRPs: map[string]lrpRow{
			// One gateway IP inside the filter, one outside it.
			"lr0-public": {Networks: []string{"192.0.2.1/24", "203.0.113.1/24"}},
		},
		LRPNameByUUID: map[string]string{"uuid-lr0-public": "lr0-public"},
		Routers: map[string]routerRow{
			"lr0": {
				LRPUUIDs: []string{"uuid-lr0-public"},
				NATs: []natRow{
					{ExternalIP: "192.0.2.10", Type: "dnat_and_snat"},   // inside the filter
					{ExternalIP: "203.0.113.10", Type: "dnat_and_snat"}, // outside the filter
				},
			},
		},
		SegmentTagByLRP: map[string]int{"lr0-public": 0},
	}
	doc := fullModeDoc(t)
	doc["network_cidr"] = anySlice([]string{"192.0.2.0/24"})

	exp := computeExpectation(snap, "gateway-1", doc, "172.20.20.4")

	// The FIP outside the filter is in no plane.
	if containsStr(exp.DesiredIPs, "203.0.113.10") {
		t.Fatalf("DesiredIPs = %v, the out-of-filter FIP must not be desired", exp.DesiredIPs)
	}
	// The in-filter FIP and both gateway IPs stay desired (gateway IPs are not
	// filtered).
	for _, ip := range []string{"192.0.2.10", "192.0.2.1", "203.0.113.1"} {
		if !containsStr(exp.DesiredIPs, ip) {
			t.Fatalf("DesiredIPs = %v, want it to include %s", exp.DesiredIPs, ip)
		}
	}
	// The manual filter is exactly the prefix-list.
	assertSet(t, "PrefixList", exp.PrefixList, []string{"192.0.2.0/24"})
	// The presence set keeps only the covered desired IPs.
	assertSet(t, "MustAnnounce", exp.MustAnnounce, []string{"192.0.2.1", "192.0.2.10"})
	if containsStr(exp.MustAnnounce, "203.0.113.1") || containsStr(exp.MustAnnounce, "203.0.113.10") {
		t.Fatalf("MustAnnounce = %v, uncovered IPs must not be required to announce", exp.MustAnnounce)
	}
	// The staleness bound is exactly the desired set.
	assertSet(t, "AnnounceBound", exp.AnnounceBound, exp.DesiredIPs)
}

// Port-forward-only mode has no OVN view at all: every OVN-driven plane is
// skipped, the desired set is the VIPs alone, and only the DNAT/FRR/announce
// planes carry anything.
func TestExpectationPortForwardOnlySkipsEveryOVNPlane(t *testing.T) {
	// A snapshot that would make gateway-1 active in full mode — to prove the
	// pf-only path ignores OVN entirely.
	snap := ovnSnapshot{
		CRPortChassis: map[string]string{"cr-lr0-public": "gateway-1"},
		LRPs:          map[string]lrpRow{"lr0-public": {Networks: []string{"192.0.2.1/24"}}},
		LRPNameByUUID: map[string]string{"uuid-lr0-public": "lr0-public"},
		Routers: map[string]routerRow{"lr0": {
			LRPUUIDs: []string{"uuid-lr0-public"},
			NATs:     []natRow{{ExternalIP: "192.0.2.10", Type: "dnat_and_snat"}},
		}},
		SegmentTagByLRP: map[string]int{"lr0-public": 0},
	}
	doc := render(t, profileGateway(t, "pf-only", "gateway-1"), "172.20.20.5")

	exp := computeExpectation(snap, "gateway-1", doc, "172.20.20.5")

	if exp.Mode != "pf-only" {
		t.Fatalf("mode = %q, want pf-only when both OVN remotes are absent", exp.Mode)
	}
	if exp.Active {
		t.Fatal("a pf-only gateway holds no OVN view and can never be active")
	}
	// Desired is the VIP alone — no FIPs, no gateway IPs.
	assertSet(t, "DesiredIPs", exp.DesiredIPs, []string{apiVIPAddr})
	// #223: the VIP is desired (kernel/announce plane) but is never an FRR
	// static — it announces through its connected route. In pf-only mode there
	// is no OVN view, so the FRR-static set is empty.
	if len(exp.FRRStatic) != 0 {
		t.Fatalf("FRRStatic = %v, want empty — the VIP announces via its connected route, not a static", exp.FRRStatic)
	}
	if !exp.SkipKernel || !exp.SkipHairpin || !exp.SkipPrefixList {
		t.Fatalf("pf-only must skip the kernel/OVS/prefix planes: skipKernel=%v skipHairpin=%v skipPrefix=%v",
			exp.SkipKernel, exp.SkipHairpin, exp.SkipPrefixList)
	}
	if exp.PrefixList != nil {
		t.Fatalf("PrefixList = %v, want it untouched (nil) in pf-only", exp.PrefixList)
	}
	if exp.MACTweakFlows != -1 {
		t.Fatalf("MACTweakFlows = %d, want -1 (skip) in pf-only", exp.MACTweakFlows)
	}
	// The presence bound is the VIP set.
	if !containsStr(exp.MustAnnounce, apiVIPAddr) {
		t.Fatalf("MustAnnounce = %v, want it to require the VIP", exp.MustAnnounce)
	}
	assertSet(t, "AnnounceBound", exp.AnnounceBound, []string{apiVIPAddr})
	// The one DNAT rule forwards the API VIP to the gateway's own backend.
	assertDNAT(t, exp.DNAT, []dnatExpectation{
		{VIP: apiVIPAddr, Proto: "tcp", Port: apiVIPPort, Backend: "172.20.20.5", DestPort: apiVIPPort},
	})
}

// A port-forward VIP on a full-mode standby gateway is dormant (#206): it gets
// no kernel route and no FRR static route, because the standby path empties the
// prefix-list that would advertise it. The route planes must therefore expect
// nothing — the whole point is that no unadvertisable static route sits in FRR
// holding inactive_routes non-zero, which the oracle's settle gate refuses to
// pass. The DNAT plane is unaffected: dormancy withholds the routes, not the
// nftables rules.
func TestExpectationDormantVIPOnStandbyGateway(t *testing.T) {
	// gateway-1 owns the only CR port, so gateway-2 is a standby — while both
	// carry the API VIP in their configuration.
	snap := ovnSnapshot{
		CRPortChassis: map[string]string{"cr-lr0-public": "gateway-1"},
		LRPs:          map[string]lrpRow{"lr0-public": {Networks: []string{"192.0.2.1/24"}}},
		LRPNameByUUID: map[string]string{"uuid-lr0-public": "lr0-public"},
		Routers: map[string]routerRow{"lr0": {
			LRPUUIDs: []string{"uuid-lr0-public"},
			NATs:     []natRow{{ExternalIP: "192.0.2.10", Type: "dnat_and_snat"}},
		}},
		Chassis:         map[string]bool{"gateway-1": true, "gateway-2": true},
		SegmentTagByLRP: map[string]int{"lr0-public": 0},
	}
	// flat-dnat is the full-mode profile that puts the API VIP on every
	// gateway, so gateway-2 carries a VIP it cannot announce.
	doc := render(t, profileGateway(t, "flat-dnat", "gateway-2"), "172.20.20.5")

	exp := computeExpectation(snap, "gateway-2", doc, "172.20.20.5")

	if exp.Mode != "full" || exp.Active {
		t.Fatalf("mode=%q active=%v, want a full-mode standby gateway", exp.Mode, exp.Active)
	}
	if len(exp.DesiredIPs) != 0 {
		t.Fatalf("DesiredIPs = %v, want none — the VIP is dormant on a standby", exp.DesiredIPs)
	}
	if len(exp.FRRStatic) != 0 {
		t.Fatalf("FRRStatic = %v, want no FRR route for a dormant VIP", exp.FRRStatic)
	}
	if len(exp.KernelRouteDev) != 0 {
		t.Fatalf("KernelRouteDev = %v, want no kernel route for a dormant VIP", exp.KernelRouteDev)
	}
	if len(exp.MustAnnounce) != 0 {
		t.Fatalf("MustAnnounce = %v, want nothing announced from a standby", exp.MustAnnounce)
	}
	// The DNAT plane still follows the config alone, so a gateway that later
	// wins the CR port only needs the announce. flat-dnat carries the hairpin
	// VIP beside the API VIP, and both rules are programmed here.
	assertDNAT(t, exp.DNAT, []dnatExpectation{
		{VIP: apiVIPAddr, Proto: "tcp", Port: apiVIPPort, Backend: "172.20.20.5", DestPort: apiVIPPort},
		{VIP: hairpinVIPAddr, Proto: "tcp", Port: hairpinVIPPort, Backend: "172.20.20.5", DestPort: apiVIPPort},
	})
	// #223: the VIP address is the announce path, so a full-mode standby
	// withholds it — the managed-VIP set is empty even though the config carries
	// manage_vip. This is the new mechanism the dormancy (#206) is enforced by.
	if len(exp.ManagedVIPs) != 0 {
		t.Fatalf("ManagedVIPs = %v, want none — a standby withholds the VIP address", exp.ManagedVIPs)
	}
}

// A port-forward VIP outside every hosted network must still be announced by
// an active gateway (#226): its own exact /32 prefix-list entry permits the
// connected route, so neither the entry nor the announce may depend on a
// hosted network covering the VIP. This is the heterogeneous blackhole — the
// flat router moves to another chassis, the VIP-carrying gateway keeps only
// its VLAN routers, and before #226 nothing exported the VIP anywhere.
func TestExpectationVIPOutsideHostedNetworksIsStillAnnounced(t *testing.T) {
	// gateway-1 is active for the VLAN-101 router alone; the flat router
	// lr0-public lives on gateway-3. gateway-1 carries the API VIP
	// (192.0.2.80), which no VLAN network covers.
	snap := ovnSnapshot{
		CRPortChassis: map[string]string{
			"cr-lr0-public":        "gateway-3",
			"cr-lr-vlan101-public": "gateway-1",
		},
		LRPs: map[string]lrpRow{
			"lr0-public":        {Networks: []string{"192.0.2.1/24"}},
			"lr-vlan101-public": {Networks: []string{"198.51.100.1/24"}},
		},
		LRPNameByUUID: map[string]string{
			"uuid-lr0-public":        "lr0-public",
			"uuid-lr-vlan101-public": "lr-vlan101-public",
		},
		Routers: map[string]routerRow{
			"lr0": {LRPUUIDs: []string{"uuid-lr0-public"}},
			"lr-vlan101": {
				LRPUUIDs: []string{"uuid-lr-vlan101-public"},
				NATs:     []natRow{{ExternalIP: "198.51.100.10", Type: "dnat_and_snat"}},
			},
		},
		Chassis:         map[string]bool{"gateway-1": true, "gateway-3": true},
		SegmentTagByLRP: map[string]int{"lr0-public": 0, "lr-vlan101-public": 101},
	}
	// The heterogeneous profile puts the API VIP on gateway-1 (#225).
	doc := render(t, profileGateway(t, "heterogeneous", "gateway-1"), "172.20.20.4")

	exp := computeExpectation(snap, "gateway-1", doc, "172.20.20.4")

	if exp.Mode != "full" || !exp.Active {
		t.Fatalf("mode=%q active=%v, want a full-mode active gateway", exp.Mode, exp.Active)
	}
	// Rule 7: the prefix-list holds the hosted VLAN network plus the VIP's own
	// exact /32 — and no entry for the flat network hosted elsewhere.
	if !containsStr(exp.PrefixList, apiVIPAddr+"/32") {
		t.Fatalf("PrefixList = %v, want the VIP's own /32 entry", exp.PrefixList)
	}
	if !containsStr(exp.PrefixList, "198.51.100.0/24") {
		t.Fatalf("PrefixList = %v, want the hosted VLAN network entry", exp.PrefixList)
	}
	if containsStr(exp.PrefixList, "192.0.2.0/24") {
		t.Fatalf("PrefixList = %v, must not expect the flat network hosted on gateway-3", exp.PrefixList)
	}
	// Rule 12: the VIP must be announced although no effective network covers
	// it — the bound that turns the heterogeneous blackhole into a sweep
	// violation instead of a probe-only failure.
	if !containsStr(exp.MustAnnounce, apiVIPAddr) {
		t.Fatalf("MustAnnounce = %v, want the VIP required regardless of network coverage", exp.MustAnnounce)
	}
	if !containsStr(exp.DesiredIPs, apiVIPAddr) {
		t.Fatalf("DesiredIPs = %v, want the VIP on an active gateway", exp.DesiredIPs)
	}
}

// With no port_forwards the agent's nftables table carries no DNAT chains and
// manages no VIP address; with the API VIP it carries exactly the one DNAT rule
// to the gateway's mgmtIP, and the hairpin-masquerade set follows the flag.
func TestExpectationWithoutPortForwardsExpectsNoDNAT(t *testing.T) {
	// An active gateway, so the effective-network set that gates the hairpin
	// masquerade rule is non-empty.
	snap := ovnSnapshot{
		CRPortChassis:   map[string]string{"cr-lr0-public": "gateway-1"},
		LRPs:            map[string]lrpRow{"lr0-public": {Networks: []string{"192.0.2.1/24"}}},
		LRPNameByUUID:   map[string]string{"uuid-lr0-public": "lr0-public"},
		Routers:         map[string]routerRow{"lr0": {LRPUUIDs: []string{"uuid-lr0-public"}}},
		SegmentTagByLRP: map[string]int{"lr0-public": 0},
	}

	t.Run("no port forwards", func(t *testing.T) {
		exp := computeExpectation(snap, "gateway-1", fullModeDoc(t), "172.20.20.4")
		if len(exp.DNAT) != 0 {
			t.Fatalf("DNAT = %v, want no rules without port_forwards", exp.DNAT)
		}
		if len(exp.ManagedVIPs) != 0 {
			t.Fatalf("ManagedVIPs = %v, want none without port_forwards", exp.ManagedVIPs)
		}
	})

	t.Run("api vip", func(t *testing.T) {
		doc := render(t, gwConfig{apiVIP: true}, "172.20.20.4")

		exp := computeExpectation(snap, "gateway-1", doc, "172.20.20.4")
		assertDNAT(t, exp.DNAT, []dnatExpectation{
			{VIP: apiVIPAddr, Proto: "tcp", Port: apiVIPPort, Backend: "172.20.20.4", DestPort: apiVIPPort},
		})
		assertSet(t, "ManagedVIPs", exp.ManagedVIPs, []string{apiVIPAddr})
		if exp.PortForwardDev != "loopback1" {
			t.Fatalf("PortForwardDev = %q, want the loopback1 default", exp.PortForwardDev)
		}
		// The profile ships the API VIP with hairpin_masquerade off.
		if len(exp.HairpinMasquerade) != 0 {
			t.Fatalf("HairpinMasquerade = %v, want none while the flag is off", exp.HairpinMasquerade)
		}

		// Turn the flag on and the VIP joins the hairpin-masquerade set.
		apiVIPIn(doc)["hairpin_masquerade"] = true
		exp = computeExpectation(snap, "gateway-1", doc, "172.20.20.4")
		assertSet(t, "HairpinMasquerade", exp.HairpinMasquerade, []string{apiVIPAddr})
	})
}

// The announced set is staleness-bounded in every mode: announced ⊆ desired
// always holds, so a FIP churned out of OVN leaves the expected set and the
// upstream may no longer announce it.
func TestExpectationAnnouncedSetIsStalenessBounded(t *testing.T) {
	base := ovnSnapshot{
		CRPortChassis:   map[string]string{"cr-lr0-public": "gateway-1"},
		LRPs:            map[string]lrpRow{"lr0-public": {Networks: []string{"192.0.2.1/24"}}},
		LRPNameByUUID:   map[string]string{"uuid-lr0-public": "lr0-public"},
		SegmentTagByLRP: map[string]int{"lr0-public": 0},
	}
	withFIP := base
	withFIP.Routers = map[string]routerRow{"lr0": {
		LRPUUIDs: []string{"uuid-lr0-public"},
		NATs:     []natRow{{ExternalIP: "192.0.2.10", Type: "dnat_and_snat"}},
	}}
	churned := base
	churned.Routers = map[string]routerRow{"lr0": {LRPUUIDs: []string{"uuid-lr0-public"}}}

	doc := fullModeDoc(t)

	t.Run("full mode active", func(t *testing.T) {
		exp := computeExpectation(withFIP, "gateway-1", doc, "172.20.20.4")
		// The bound is exactly the desired set, and the presence set is within it.
		assertSet(t, "AnnounceBound", exp.AnnounceBound, exp.DesiredIPs)
		if !subset(exp.MustAnnounce, exp.AnnounceBound) {
			t.Fatalf("MustAnnounce %v is not within AnnounceBound %v", exp.MustAnnounce, exp.AnnounceBound)
		}
		if !containsStr(exp.AnnounceBound, "192.0.2.10") {
			t.Fatalf("AnnounceBound = %v, want it to carry the live FIP", exp.AnnounceBound)
		}
	})

	t.Run("churned FIP leaves the bound", func(t *testing.T) {
		exp := computeExpectation(churned, "gateway-1", doc, "172.20.20.4")
		if containsStr(exp.AnnounceBound, "192.0.2.10") {
			t.Fatalf("AnnounceBound = %v, the churned-away FIP must have left it", exp.AnnounceBound)
		}
		assertSet(t, "AnnounceBound", exp.AnnounceBound, exp.DesiredIPs)
	})

	t.Run("full mode standby", func(t *testing.T) {
		exp := computeExpectation(withFIP, "gateway-2", doc, "172.20.20.5")
		// A standby node desires nothing, so the bound is empty and the subset
		// relation still holds trivially.
		assertSet(t, "AnnounceBound", exp.AnnounceBound, exp.DesiredIPs)
		if !subset(exp.MustAnnounce, exp.AnnounceBound) {
			t.Fatalf("MustAnnounce %v is not within AnnounceBound %v", exp.MustAnnounce, exp.AnnounceBound)
		}
	})

	t.Run("port-forward only", func(t *testing.T) {
		pfDoc := render(t, profileGateway(t, "pf-only", "gateway-1"), "172.20.20.5")
		exp := computeExpectation(withFIP, "gateway-1", pfDoc, "172.20.20.5")
		// The bound is the VIP set, and the presence set matches it.
		assertSet(t, "AnnounceBound", exp.AnnounceBound, []string{apiVIPAddr})
		assertSet(t, "AnnounceBound", exp.AnnounceBound, exp.DesiredIPs)
		if !subset(exp.MustAnnounce, exp.AnnounceBound) {
			t.Fatalf("MustAnnounce %v is not within AnnounceBound %v", exp.MustAnnounce, exp.AnnounceBound)
		}
	})
}

// fullModeDoc is the baked gwnode config parsed into a doc — both OVN remotes
// present, no port_forwards, no manual filter — the full-mode starting point
// the per-test overlays build on.
func fullModeDoc(t *testing.T) map[string]any {
	t.Helper()
	doc, err := parseConfig(baseConfig(t))
	if err != nil {
		t.Fatalf("parseConfig(baseConfig): %v", err)
	}
	return doc
}

// assertSet fails unless got equals want as an ordered set (both come sorted
// out of the oracle / test literals).
func assertSet(t *testing.T, name string, got, want []string) {
	t.Helper()
	if !equalStrings(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func assertDevs(t *testing.T, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("KernelRouteDev = %v, want %v", got, want)
	}
	for ip, dev := range want {
		if got[ip] != dev {
			t.Fatalf("KernelRouteDev[%s] = %q, want %q", ip, got[ip], dev)
		}
	}
}

func assertDNAT(t *testing.T, got, want []dnatExpectation) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("DNAT = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("DNAT[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func subset(sub, super []string) bool {
	set := make(map[string]bool, len(super))
	for _, s := range super {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
