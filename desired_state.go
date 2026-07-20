package main

import (
	"net"
	"sort"
)

// =============================================================================
// Desired-state computation
// =============================================================================
//
// Everything in this file is a pure function of an OVNState snapshot plus the
// configured port forwards: no OVSDB client, no kernel, no OVS, no logging of
// effects. reconcile calls computeDesiredState once and then only *acts* on the
// result, which keeps the derivation — including the IPv4/dual-stack fork that
// decides what gets announced — directly unit-testable.

// desiredState is everything reconcile needs to derive from OVN before it
// touches the data plane.
type desiredState struct {
	// HairpinTargets maps each IP needing a hairpin flow to the MAC of the
	// router port that owns it and the localnet segment its external network
	// is on. Deliberately dual-stack (see HairpinIPs).
	HairpinTargets map[string]HairpinTarget

	// Segments is the deduplicated set of localnet segments the locally-active
	// routers need a data plane for, sorted by localnet port name.
	Segments []DesiredSegment

	// SegmentByName indexes Segments by localnet port name. EnsureSegments
	// iterates the ordered slice; the per-IP kernel-route decision needs a
	// lookup, so both views of the same set are kept.
	SegmentByName map[string]DesiredSegment

	// HairpinIPs is the IPv4-only subset of HairpinTargets' keys, sorted.
	HairpinIPs []string

	// ExcludedNonV4 lists the HairpinTargets keys dropped from HairpinIPs
	// because they are not IPv4, sorted. Reported by reconcile at debug level.
	ExcludedNonV4 []string

	// DesiredIPs is HairpinIPs plus the port-forward VIPs, deduplicated and
	// sorted. This is the set that gets kernel routes and FRR announcements.
	DesiredIPs []string

	// DormantVIPs lists the configured port-forward VIPs left out of
	// DesiredIPs because this node cannot advertise them, sorted. Reported by
	// reconcile at info level on change.
	DormantVIPs []string
}

// buildHairpinTargets collects every IP that needs a hairpin flow: the NAT
// external IPs (FIPs and SNAT IPs) and the router gateway IPs (LRP networks).
// Port-forward VIPs are intentionally excluded — their DNAT is handled by
// nftables.
//
// The MAC is used as mod_dl_dst in the hairpin flow so OVN's L2 lookup delivers
// the reflected packet to the correct router; the segment selects the patch port
// the flow binds to. An LRP with no MAC contributes no gateway target: a flow
// cannot set a dl_dst it does not know.
func buildHairpinTargets(state OVNState) map[string]HairpinTarget {
	targets := make(map[string]HairpinTarget, len(state.NATIPToRouterMAC))
	for ip, mac := range state.NATIPToRouterMAC {
		targets[ip] = HairpinTarget{RouterMAC: mac, Segment: state.NATIPToSegment[ip]}
	}
	// Router gateway IPs (LRP networks) are included so that VMs on a
	// same-chassis router can reach other routers' gateway addresses,
	// matching the behaviour seen from outside.
	for _, lr := range state.LocalRouters {
		for _, cidr := range lr.LRPNetworks {
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			if lr.LRPMAC != "" {
				targets[ip.String()] = HairpinTarget{RouterMAC: lr.LRPMAC, Segment: segmentName(lr.Segment)}
			}
		}
	}
	return targets
}

// buildDesiredSegments returns the deduplicated set of localnet segments the
// locally-active routers need a data plane for — as a slice sorted by localnet
// port name, plus the same set indexed by that name. Routers whose segment is
// unresolved contribute the "" fallback segment.
func buildDesiredSegments(localRouters []LocalRouterInfo) ([]DesiredSegment, map[string]DesiredSegment) {
	segmentSet := make(map[string]DesiredSegment)
	for _, lr := range localRouters {
		key := segmentName(lr.Segment)
		if _, ok := segmentSet[key]; ok {
			continue
		}
		d := DesiredSegment{LocalnetPort: key}
		if lr.Segment != nil {
			d.VLANTag = lr.Segment.VLANTag
		}
		segmentSet[key] = d
	}
	segments := make([]DesiredSegment, 0, len(segmentSet))
	for _, d := range segmentSet {
		segments = append(segments, d)
	}
	sort.Slice(segments, func(i, j int) bool {
		return segments[i].LocalnetPort < segments[j].LocalnetPort
	})
	return segments, segmentSet
}

// splitIPv4 partitions the hairpin target IPs into the IPv4 addresses that feed
// the route/announce plane and the non-IPv4 ones that do not. Both are sorted.
//
// This is the single choke point where the IPv4-only route/announce plane forks
// off the deliberately dual-stack hairpin targets. Everything downstream
// (AddKernelRoute, AddFRRRoutes, BGP, the takeover marker) is IPv4-only, so a v6
// address would fail the FRR batch and block the marker. The OVS hairpin plane
// keeps the v6 targets (#54); full IPv6 route support is tracked in #85/#70.
func splitIPv4(targets map[string]HairpinTarget) (v4, nonV4 []string) {
	v4 = make([]string, 0, len(targets))
	for ip := range targets {
		if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
			nonV4 = append(nonV4, ip)
			continue
		}
		v4 = append(v4, ip)
	}
	sort.Strings(v4)
	sort.Strings(nonV4)
	return v4, nonV4
}

// computeDesiredState derives everything reconcile needs from an OVN snapshot
// and the configured port forwards. In port-forward-only mode state is the zero
// OVNState, so the result reduces to the VIPs alone.
//
// announceVIPs decides whether the configured port-forward VIPs join the route
// plane this cycle; reconcile derives it from whether this node maintains the
// FRR prefix-list that would permit them (see vipRoutesAnnounceable). When it is
// false the VIPs are reported as DormantVIPs instead: no kernel route, no FRR
// static route, and nothing for the inactive-route check to alarm on.
func computeDesiredState(state OVNState, portForwards []PortForwardVIP, announceVIPs bool) desiredState {
	targets := buildHairpinTargets(state)
	hairpinIPs, excludedNonV4 := splitIPv4(targets)
	segments, segmentByName := buildDesiredSegments(state.LocalRouters)

	// desiredIPs extends hairpinIPs with the port-forward VIPs — these need
	// kernel routes on br-ex and FRR static routes for BGP announcement.
	desiredIPs := make([]string, 0, len(hairpinIPs)+len(portForwards))
	desiredIPs = append(desiredIPs, hairpinIPs...)
	var dormantVIPs []string
	for _, pf := range portForwards {
		if !announceVIPs {
			dormantVIPs = append(dormantVIPs, pf.VIP)
			continue
		}
		desiredIPs = append(desiredIPs, pf.VIP)
	}

	return desiredState{
		HairpinTargets: targets,
		Segments:       segments,
		SegmentByName:  segmentByName,
		HairpinIPs:     hairpinIPs,
		ExcludedNonV4:  excludedNonV4,
		DesiredIPs:     uniqueIPs(desiredIPs),
		DormantVIPs:    uniqueIPs(dormantVIPs),
	}
}
