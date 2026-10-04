package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
)

// The route watch subscribes to the kernel's route notifications and makes
// the agent reconcile as soon as a route it owns is deleted or replaced from
// outside, instead of leaving the gap open until the next periodic reconcile.
// This file holds the platform-independent part: what counts as drift. The
// goroutine that turns drift into a reconcile is the driftWatcher of
// drift_watch.go, and the netlink subscription is in routing_linux.go.

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

// watchSubject names the route watch in its log lines.
func (routeEvent) watchSubject() string { return "route" }

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
var errRouteWatchUnsupported = fmt.Errorf("route watch is only supported on Linux: %w", errDriftWatchUnsupported)

// errRouteSubscriptionClosed is the error the outage warning carries when a
// subscription ended because its event channel was closed.
var errRouteSubscriptionClosed = errors.New("kernel route subscription closed")

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

// routeWatcher turns kernel route events into reconcile triggers. The embedded
// driftWatcher owns the subscription on one goroutine, started by start. The
// reconcile publishes what it owns through setOwned.
//
// setOwned, start and wait are no-ops on a nil receiver, so an agent without a
// watcher (route_watch off, dry-run, port-forward-only mode) calls them
// unguarded.
type routeWatcher struct {
	// mu guards owned.
	mu    sync.Mutex
	owned routeOwnership

	vethNexthop string

	driftWatcher[routeEvent]
}

// newRouteWatcher builds a watcher that calls trigger when an owned route
// drifts. It copies the three settings it needs out of cfg and keeps no
// Config: the watcher's goroutine must never read the agent's configuration,
// which a reload replaces from Run's goroutine.
func newRouteWatcher(cfg Config, trigger func()) *routeWatcher {
	routeTableID, vrfName := cfg.RouteTableID, cfg.VRFName
	w := &routeWatcher{vethNexthop: cfg.VethNexthop}
	w.driftWatcher = driftWatcher[routeEvent]{
		kinds:     []string{routeDriftKindKernel, routeDriftKindFRR},
		closedErr: errRouteSubscriptionClosed,
		subscribe: func(done <-chan struct{}) (<-chan routeEvent, error) {
			return subscribeRouteEvents(done, routeTableID, vrfName)
		},
		classify: func(ev routeEvent) (string, bool) {
			return classifyRouteEvent(ev, w.currentOwned(), w.vethNexthop)
		},
		onDrift: func(ev routeEvent, kind string) {
			recordRouteDrift(kind)
			slog.Debug("route drift event", "kind", kind, "dst", ev.Dst, "deleted", ev.Deleted, "replaced", ev.Replaced)
		},
		trigger:     trigger,
		debounce:    driftDebounce,
		minInterval: driftMinInterval,
		backoffMin:  driftMinBackoff,
		backoffMax:  driftMaxBackoff,
	}
	return w
}

// setOwned publishes the routes the current reconcile wants in place. The
// reconcile calls it before its first removal, so a route it deletes on
// purpose is no longer owned when the kernel reports the deletion.
func (w *routeWatcher) setOwned(o routeOwnership) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.owned = o
	w.mu.Unlock()
}

// currentOwned returns the last published ownership.
func (w *routeWatcher) currentOwned() routeOwnership {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.owned
}

// start runs the watcher on its own goroutine until ctx is done.
func (w *routeWatcher) start(ctx context.Context) {
	if w == nil {
		return
	}
	w.driftWatcher.start(ctx)
}

// wait blocks until the goroutine has returned. It returns at once when start
// was never called.
func (w *routeWatcher) wait() {
	if w == nil {
		return
	}
	w.driftWatcher.wait()
}
