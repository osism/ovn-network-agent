package main

import (
	"errors"
	"net"
	"strings"
)

// The route watch subscribes to the kernel's route notifications and makes
// the agent reconcile as soon as a route it owns is deleted or replaced from
// outside, instead of leaving the gap open until the next periodic reconcile.
// This file holds the platform-independent part: what counts as drift. The
// netlink subscription is in routing_linux.go.

// routeEvent is one kernel route notification, reduced to what the drift
// decision needs. KernelTable and VRFTable are both true when route_table_id
// names the VRF's own table.
type routeEvent struct {
	Deleted     bool   // RTM_DELROUTE; false means RTM_NEWROUTE
	Replaced    bool   // RTM_NEWROUTE that carried NLM_F_REPLACE
	KernelTable bool   // the table AddKernelRoute writes
	VRFTable    bool   // the routing table of the VRF device
	AgentProto  bool   // route protocol is rtProtoOVNNetworkAgent
	Dst         string // IPv4 CIDR, e.g. "192.0.2.10/32"
	Gw          string // gateway of a single-path route, else ""
}

// routeOwnership is the set of routes the last reconcile wanted in place. An
// event for any other prefix is not drift, which is how the routes a reconcile
// removes on purpose stay out of the count: they leave the ownership before
// they are deleted.
type routeOwnership struct {
	KernelIPs map[string]bool // /32 kernel routes, keyed by IP
	FRRIPs    map[string]bool // FRR statics, keyed by IP
	LeakNets  map[string]bool // veth-leak network routes, keyed by CIDR
}

// errRouteWatchUnsupported is what subscribeRouteEvents returns on a platform
// without kernel route notifications.
var errRouteWatchUnsupported = errors.New("route watch is only supported on Linux")

// Values of the kind label of route_drift_total.
const (
	routeDriftKindKernel = "kernel"
	routeDriftKindFRR    = "frr"
)

// newRouteOwnership builds the ownership one reconcile publishes. The maps are
// never written again, so a reader may keep using a copy of the struct without
// holding a lock.
func newRouteOwnership(kernelIPs, frrIPs []string, leakNets []*net.IPNet) routeOwnership {
	o := routeOwnership{
		KernelIPs: make(map[string]bool, len(kernelIPs)),
		FRRIPs:    make(map[string]bool, len(frrIPs)),
		LeakNets:  make(map[string]bool, len(leakNets)),
	}
	for _, ip := range kernelIPs {
		o.KernelIPs[ip] = true
	}
	for _, ip := range frrIPs {
		o.FRRIPs[ip] = true
	}
	for _, n := range leakNets {
		o.LeakNets[n.String()] = true
	}
	return o
}

// classifyRouteEvent reports whether ev is a change to an owned route that the
// agent did not make, and which kind of route it hit. The cases are checked in
// order and the first match wins:
//
//  1. a deleted agent /32 in the kernel table
//  2. a deleted agent leak-network route in the VRF table
//  3. a deleted /32 of an owned FRR static in the VRF table, as zebra
//     installed it
//  4. a foreign route that replaced an agent /32 in the kernel table
//  5. a foreign route that replaced an owned FRR static in the VRF table with
//     a next hop other than vethNexthop
//
// Case 4 exists because a replace emits only RTM_NEWROUTE, and the foreign
// route is invisible to ListKernelRoutes, so the reconcile sees the agent's
// route as missing and puts it back. Case 5 mirrors ListFRRRoutes, which
// counts a static as the agent's only when it points at vethNexthop. An empty
// Gw (multipath, or a kernel that reports only a nexthop id) is ignored rather
// than guessed at.
//
// The protocol zebra tags its routes with is not relied on: cases 3 and 5 only
// require that it is not the agent's. The agent's own writes never match. Its
// adds carry the agent protocol, and zebra's re-install of an owned static
// carries vethNexthop.
func classifyRouteEvent(ev routeEvent, owned routeOwnership, vethNexthop string) (kind string, drift bool) {
	ip, isHost := strings.CutSuffix(ev.Dst, "/32")

	if ev.Deleted {
		switch {
		case ev.KernelTable && ev.AgentProto && isHost && owned.KernelIPs[ip]:
			return routeDriftKindKernel, true
		case ev.VRFTable && ev.AgentProto && owned.LeakNets[ev.Dst]:
			return routeDriftKindKernel, true
		case ev.VRFTable && !ev.AgentProto && isHost && owned.FRRIPs[ip]:
			return routeDriftKindFRR, true
		}
		return "", false
	}

	if !ev.Replaced || ev.AgentProto || !isHost {
		return "", false
	}
	switch {
	case ev.KernelTable && owned.KernelIPs[ip]:
		return routeDriftKindKernel, true
	case ev.VRFTable && owned.FRRIPs[ip] && ev.Gw != "" && ev.Gw != vethNexthop:
		return routeDriftKindFRR, true
	}
	return "", false
}
