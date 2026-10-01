package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"
)

// The route watch subscribes to the kernel's route notifications and makes
// the agent reconcile as soon as a route it owns is deleted or replaced from
// outside, instead of leaving the gap open until the next periodic reconcile.
// This file holds the platform-independent part: what counts as drift, and the
// goroutine that turns drift into a reconcile. The netlink subscription is in
// routing_linux.go.

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

// errRouteSubscriptionClosed is the error the outage warning carries when a
// subscription ended because its event channel was closed.
var errRouteSubscriptionClosed = errors.New("kernel route subscription closed")

const (
	// routeDriftDebounce is how long the watcher collects drift events before
	// it triggers the reconcile, so a flush of many routes is one cycle.
	routeDriftDebounce = 100 * time.Millisecond

	// routeDriftMinInterval is the shortest time between two triggers. It
	// keeps a persistent fault from spinning the agent: a competing writer, or
	// AddKernelRoute rolling its own route back when the ip rule fails,
	// produces a drift event per reconcile.
	routeDriftMinInterval = time.Second

	// routeWatchBackoffMin and routeWatchBackoffMax bound the wait before the
	// watcher subscribes again after a failed subscribe or a failed socket.
	// routeWatchBackoffMax is also how long a subscription has to hold before
	// the wait starts over at the minimum.
	routeWatchBackoffMin = time.Second
	routeWatchBackoffMax = 30 * time.Second
)

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

// routeWatcher turns kernel route events into reconcile triggers. One
// goroutine, started by start, owns the subscription. The reconcile publishes
// what it owns through setOwned.
//
// setOwned, start and wait are no-ops on a nil receiver, so an agent without a
// watcher (route_watch off, dry-run, port-forward-only mode) calls them
// unguarded.
type routeWatcher struct {
	// mu guards owned.
	mu    sync.Mutex
	owned routeOwnership

	vethNexthop string
	trigger     func()
	// subscribe opens one subscription. The watcher closes done when it is
	// finished with that subscription.
	subscribe func(done <-chan struct{}) (<-chan routeEvent, error)

	debounce    time.Duration
	minInterval time.Duration
	backoffMin  time.Duration
	backoffMax  time.Duration

	// stopped is closed when the goroutine has returned. Nil until start.
	stopped chan struct{}
}

// newRouteWatcher builds a watcher that calls trigger when an owned route
// drifts. It copies the three settings it needs out of cfg and keeps no
// Config: the watcher's goroutine must never read the agent's configuration,
// which a reload replaces from Run's goroutine.
func newRouteWatcher(cfg Config, trigger func()) *routeWatcher {
	routeTableID, vrfName := cfg.RouteTableID, cfg.VRFName
	return &routeWatcher{
		vethNexthop: cfg.VethNexthop,
		trigger:     trigger,
		subscribe: func(done <-chan struct{}) (<-chan routeEvent, error) {
			return subscribeRouteEvents(done, routeTableID, vrfName)
		},
		debounce:    routeDriftDebounce,
		minInterval: routeDriftMinInterval,
		backoffMin:  routeWatchBackoffMin,
		backoffMax:  routeWatchBackoffMax,
	}
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
	w.stopped = make(chan struct{})
	go func() {
		defer close(w.stopped)
		w.run(ctx)
	}()
}

// wait blocks until the goroutine has returned. It returns at once when start
// was never called.
func (w *routeWatcher) wait() {
	if w == nil || w.stopped == nil {
		return
	}
	<-w.stopped
}

// run subscribes, handles the events of that subscription, and subscribes
// again with a backoff when the subscription fails. A watcher that cannot
// subscribe leaves the agent where it is without one: on the periodic
// reconcile.
func (w *routeWatcher) run(ctx context.Context) {
	var (
		// backoff is the wait before the previous subscribe, 0 before the
		// first.
		backoff time.Duration
		// outage is true from the warning about a lost subscription until
		// the next subscribe that succeeds, so one outage logs one warning
		// however many retries it takes.
		outage      bool
		lastTrigger time.Time
	)
	for ctx.Err() == nil {
		// done belongs to this subscription alone. It is closed on every way
		// out, which is what releases the socket of a subscription whose
		// channel was closed.
		done := make(chan struct{})
		events, err := w.subscribe(done)
		if errors.Is(err, errRouteWatchUnsupported) {
			close(done)
			slog.Debug("route watch is not supported on this platform")
			return
		}
		// lived is how long this subscription held, 0 when the subscribe
		// failed.
		var lived time.Duration
		if err == nil {
			if outage {
				// Route changes during the outage were not seen.
				outage = false
				slog.Info("route watch is back")
				lastTrigger = time.Now()
				w.trigger()
			}
			subscribedAt := time.Now()
			lastTrigger = w.consume(ctx, events, lastTrigger)
			lived = time.Since(subscribedAt)
			err = errRouteSubscriptionClosed
		}
		close(done)
		if ctx.Err() != nil {
			return
		}

		if !outage {
			outage = true
			slog.Warn("route watch unavailable, drift is repaired by the periodic reconcile until it is back", "error", err)
		}
		backoff = nextBackoff(backoff, lived, w.backoffMin, w.backoffMax)
		retry := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			retry.Stop()
			return
		case <-retry.C:
		}
	}
}

// nextBackoff returns the wait before the next subscribe. prev is the wait
// before the subscribe that just ended, 0 for the first one. lived is how long
// its subscription held, 0 when the subscribe failed.
//
// The wait doubles up to hi. It starts over at lo only after a subscription
// that held for hi, which is what counts as healthy. One that opens and fails
// right away keeps backing off: every resubscribe after an outage triggers a
// reconcile, so a flapping socket would otherwise reconcile the agent every
// lo.
func nextBackoff(prev, lived, lo, hi time.Duration) time.Duration {
	if prev == 0 || lived >= hi {
		return lo
	}
	return min(2*prev, hi)
}

// consume handles the events of one subscription until its channel is closed
// or ctx is done. It takes the time of the previous trigger and returns the
// time of the last one, so the rate limit holds across subscriptions.
//
// Drift events arm one timer. It fires after the debounce, or later when that
// is needed to keep minInterval since the previous trigger. Events that arrive
// while it is pending ride along.
func (w *routeWatcher) consume(ctx context.Context, events <-chan routeEvent, lastTrigger time.Time) time.Time {
	var (
		timer       *time.Timer
		fire        <-chan time.Time
		kernel, frr int
	)
	fireTrigger := func() {
		timer, fire = nil, nil
		if ctx.Err() != nil {
			return
		}
		slog.Info("route drift detected, reconciling", "kernel", kernel, "frr", frr)
		kernel, frr = 0, 0
		lastTrigger = time.Now()
		w.trigger()
	}

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return lastTrigger

		case ev, ok := <-events:
			if !ok {
				// The socket failed, so later changes go unseen. A pending
				// trigger does not wait for its timer.
				if timer != nil {
					timer.Stop()
					fireTrigger()
				}
				return lastTrigger
			}
			kind, drift := classifyRouteEvent(ev, w.currentOwned(), w.vethNexthop)
			if !drift {
				continue
			}
			recordRouteDrift(kind)
			slog.Debug("route drift event", "kind", kind, "dst", ev.Dst, "deleted", ev.Deleted, "replaced", ev.Replaced)
			if kind == routeDriftKindFRR {
				frr++
			} else {
				kernel++
			}
			if timer == nil {
				timer = time.NewTimer(max(w.debounce, w.minInterval-time.Since(lastTrigger)))
				fire = timer.C
			}

		case <-fire:
			fireTrigger()
		}
	}
}
