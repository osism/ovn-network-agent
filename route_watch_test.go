package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testWatchFIP     = "192.0.2.10"
	testWatchVIP     = "192.0.2.20"
	testWatchNexthop = "169.254.0.1"
	testWatchLeakNet = "192.0.2.0/24"
)

// testRouteOwnership owns one FIP as a kernel route and as an FRR static, one
// port-forward VIP as a kernel route only, and one veth-leak network. The VIP
// keeps the two sets apart, as they are in production.
func testRouteOwnership(t *testing.T) routeOwnership {
	t.Helper()
	_, leak, err := net.ParseCIDR(testWatchLeakNet)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", testWatchLeakNet, err)
	}
	return newRouteOwnership([]string{testWatchFIP, testWatchVIP}, []string{testWatchFIP}, []*net.IPNet{leak})
}

func TestClassifyRouteEvent(t *testing.T) {
	const (
		fipDst = testWatchFIP + "/32"
		vipDst = testWatchVIP + "/32"
	)
	owned := testRouteOwnership(t)

	cases := []struct {
		name      string
		ev        routeEvent
		owned     routeOwnership
		wantKind  string
		wantDrift bool
	}{
		{
			name:      "case 1: deleted agent /32 in the kernel table",
			ev:        routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: fipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "case 2: deleted agent leak network in the VRF table",
			ev:        routeEvent{Deleted: true, VRFTable: true, AgentProto: true, Dst: testWatchLeakNet, Gw: testWatchNexthop},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "case 3: deleted FRR static in the VRF table",
			ev:        routeEvent{Deleted: true, VRFTable: true, Dst: fipDst, Gw: testWatchNexthop},
			owned:     owned,
			wantKind:  "frr",
			wantDrift: true,
		},
		{
			name:      "case 4: foreign route replaced the agent /32 in the kernel table",
			ev:        routeEvent{Replaced: true, KernelTable: true, Dst: fipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "case 5: foreign route replaced the FRR static with another next hop",
			ev:        routeEvent{Replaced: true, VRFTable: true, Dst: fipDst, Gw: "198.51.100.1"},
			owned:     owned,
			wantKind:  "frr",
			wantDrift: true,
		},
		{
			name:      "both tables: a delete of the agent /32 is the kernel case, not the FRR case",
			ev:        routeEvent{Deleted: true, KernelTable: true, VRFTable: true, AgentProto: true, Dst: fipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "both tables: a foreign replace is the kernel case, not the FRR case",
			ev:        routeEvent{Replaced: true, KernelTable: true, VRFTable: true, Dst: fipDst, Gw: "198.51.100.1"},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "kernel-only VIP: a delete of the agent /32 in the kernel table",
			ev:        routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: vipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:  "kernel-only VIP: a delete of a non-agent /32 in the VRF table",
			ev:    routeEvent{Deleted: true, VRFTable: true, Dst: vipDst, Gw: testWatchNexthop},
			owned: owned,
		},
		{
			name:  "kernel-only VIP: a foreign replace in the VRF table with another next hop",
			ev:    routeEvent{Replaced: true, VRFTable: true, Dst: vipDst, Gw: "198.51.100.1"},
			owned: owned,
		},
		{
			name:  "delete of a /32 that is not owned",
			ev:    routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: "192.0.2.99/32"},
			owned: owned,
		},
		{
			name:  "delete in the kernel table without the agent protocol",
			ev:    routeEvent{Deleted: true, KernelTable: true, Dst: fipDst},
			owned: owned,
		},
		{
			name:  "delete of a leak network that is not owned",
			ev:    routeEvent{Deleted: true, VRFTable: true, AgentProto: true, Dst: "198.51.100.0/24"},
			owned: owned,
		},
		{
			name:  "delete of the leak network without the agent protocol",
			ev:    routeEvent{Deleted: true, VRFTable: true, Dst: testWatchLeakNet},
			owned: owned,
		},
		{
			name:  "new route without replace",
			ev:    routeEvent{KernelTable: true, Dst: fipDst},
			owned: owned,
		},
		{
			name:  "replace by the agent itself",
			ev:    routeEvent{Replaced: true, KernelTable: true, AgentProto: true, Dst: fipDst},
			owned: owned,
		},
		{
			name:  "replace in the VRF table that keeps the veth next hop",
			ev:    routeEvent{Replaced: true, VRFTable: true, Dst: fipDst, Gw: testWatchNexthop},
			owned: owned,
		},
		{
			name:  "replace in the VRF table without a gateway",
			ev:    routeEvent{Replaced: true, VRFTable: true, Dst: fipDst},
			owned: owned,
		},
		{
			name: "zero-value ownership",
			ev:   routeEvent{Deleted: true, KernelTable: true, VRFTable: true, AgentProto: true, Dst: fipDst},
		},
		{
			name:  "empty ownership",
			ev:    routeEvent{Deleted: true, KernelTable: true, VRFTable: true, AgentProto: true, Dst: fipDst},
			owned: newRouteOwnership(nil, nil, nil),
		},
		{
			name:  "empty ownership, foreign replace",
			ev:    routeEvent{Replaced: true, KernelTable: true, VRFTable: true, Dst: fipDst, Gw: "198.51.100.1"},
			owned: newRouteOwnership(nil, nil, nil),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, drift := classifyRouteEvent(tc.ev, tc.owned, testWatchNexthop)
			if kind != tc.wantKind || drift != tc.wantDrift {
				t.Errorf("classifyRouteEvent() = (%q, %v), want (%q, %v)", kind, drift, tc.wantKind, tc.wantDrift)
			}
		})
	}
}

const routeDriftMetric = "ovn_network_agent_route_drift_total"

// ownedKernelDelete is the event for the test FIP's kernel route being deleted
// from outside. unownedKernelDelete is the same event for an address the test
// ownership does not hold.
var (
	ownedKernelDelete   = routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: testWatchFIP + "/32"}
	unownedKernelDelete = routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: "192.0.2.99/32"}
)

// triggerLog records when the watcher called its trigger.
type triggerLog struct {
	mu sync.Mutex
	at []time.Time
}

func (l *triggerLog) trigger() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at = append(l.at, time.Now())
}

func (l *triggerLog) times() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]time.Time(nil), l.at...)
}

func (l *triggerLog) count() int { return len(l.times()) }

// subscribeResult is what one call of a scripted subscribe returns.
type subscribeResult struct {
	events chan routeEvent
	err    error
}

// subscribeScript stands in for the netlink subscription. Each call returns
// the next scripted result. Once the script is used up a call returns a
// channel that never delivers, so the watcher idles on it.
type subscribeScript struct {
	mu      sync.Mutex
	results []subscribeResult
	at      []time.Time // when each call came in
}

func (s *subscribeScript) subscribe(<-chan struct{}) (<-chan routeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = append(s.at, time.Now())
	if len(s.results) == 0 {
		return make(chan routeEvent), nil
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r.events, r.err
}

func (s *subscribeScript) callTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.at...)
}

func (s *subscribeScript) callCount() int { return len(s.callTimes()) }

// newTestRouteWatcher builds a watcher that owns the test FIP, subscribes
// through the script and runs on short timings. The test adjusts the timings
// it is about before it starts the watcher.
func newTestRouteWatcher(t *testing.T, script *subscribeScript, triggers *triggerLog) *routeWatcher {
	t.Helper()
	w := newRouteWatcher(Config{VethNexthop: testWatchNexthop}, triggers.trigger)
	w.subscribe = script.subscribe
	w.debounce = 20 * time.Millisecond
	w.minInterval = time.Millisecond
	w.backoffMin = 10 * time.Millisecond
	w.backoffMax = 40 * time.Millisecond
	w.setOwned(testRouteOwnership(t))
	return w
}

// startRouteWatcher starts w and returns the function that stops it and waits
// for its goroutine. The function also runs at the end of the test. Call
// withTestMetrics and captureSlog before this, so their cleanups run after the
// goroutine is gone.
func startRouteWatcher(t *testing.T, w *routeWatcher) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w.start(ctx)
	stop = func() {
		cancel()
		w.wait()
	}
	t.Cleanup(stop)
	return stop
}

// sendRouteEvent hands ev to the watcher. The channel is unbuffered, so the
// watcher has taken the event when this returns.
func sendRouteEvent(t *testing.T, events chan<- routeEvent, ev routeEvent) {
	t.Helper()
	select {
	case events <- ev:
	case <-time.After(2 * time.Second):
		t.Fatalf("the watcher did not take the event %+v", ev)
	}
}

// waitForCondition polls cond until it holds and fails the test after 2 s.
func waitForCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// returnsWithin reports whether fn returned before the timeout.
func returnsWithin(d time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func TestRouteWatcherDebouncesDriftIntoOneTrigger(t *testing.T) {
	m := withTestMetrics(t)
	events := make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: events}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	w.debounce = 100 * time.Millisecond
	startRouteWatcher(t, w)

	first := time.Now()
	for range 3 {
		sendRouteEvent(t, events, ownedKernelDelete)
	}

	waitForCondition(t, "the debounced trigger", func() bool { return triggers.count() > 0 })
	if waited := triggers.times()[0].Sub(first); waited < w.debounce {
		t.Errorf("trigger came %v after the first event, want at least the debounce of %v", waited, w.debounce)
	}
	// With a rate limit of 1 ms, three triggers would be out long before
	// two more debounce windows have passed.
	time.Sleep(2 * w.debounce)
	if got := triggers.count(); got != 1 {
		t.Errorf("triggers = %d, want 1 for three events inside the debounce window", got)
	}
	if got := counterValue(t, m, routeDriftMetric, "kind", "kernel"); got != 3 {
		t.Errorf("%s{kind=\"kernel\"} = %v, want 3", routeDriftMetric, got)
	}
}

func TestRouteWatcherIgnoresUnownedPrefix(t *testing.T) {
	m := withTestMetrics(t)
	events := make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: events}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	startRouteWatcher(t, w)

	sendRouteEvent(t, events, unownedKernelDelete)

	time.Sleep(2 * w.debounce)
	if got := triggers.count(); got != 0 {
		t.Errorf("triggers = %d, want 0 for a prefix that is not owned", got)
	}
	for _, kind := range []string{"kernel", "frr"} {
		if got := counterValue(t, m, routeDriftMetric, "kind", kind); got != 0 {
			t.Errorf("%s{kind=%q} = %v, want 0", routeDriftMetric, kind, got)
		}
	}
}

// An event counts against the ownership of the moment it arrives, so a prefix
// the reconcile dropped stops being drift without a restart of the watcher.
func TestRouteWatcherFollowsPublishedOwnership(t *testing.T) {
	withTestMetrics(t)
	events := make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: events}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	startRouteWatcher(t, w)

	w.setOwned(newRouteOwnership(nil, nil, nil))
	sendRouteEvent(t, events, ownedKernelDelete)
	time.Sleep(2 * w.debounce)
	if got := triggers.count(); got != 0 {
		t.Fatalf("triggers = %d, want 0 after the ownership was emptied", got)
	}

	w.setOwned(testRouteOwnership(t))
	sendRouteEvent(t, events, ownedKernelDelete)
	waitForCondition(t, "the trigger for the re-owned prefix", func() bool { return triggers.count() == 1 })
}

func TestRouteWatcherKeepsMinIntervalBetweenTriggers(t *testing.T) {
	withTestMetrics(t)
	events := make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: events}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	w.minInterval = 300 * time.Millisecond
	startRouteWatcher(t, w)

	sendRouteEvent(t, events, ownedKernelDelete)
	waitForCondition(t, "the first trigger", func() bool { return triggers.count() == 1 })
	sendRouteEvent(t, events, ownedKernelDelete)
	waitForCondition(t, "the second trigger", func() bool { return triggers.count() == 2 })

	at := triggers.times()
	if gap := at[1].Sub(at[0]); gap < w.minInterval {
		t.Errorf("second trigger came %v after the first, want at least %v", gap, w.minInterval)
	}
}

// The rate limit also holds between the trigger of a recovered subscription
// and the first drift that subscription reports.
func TestRouteWatcherKeepsMinIntervalAcrossSubscriptions(t *testing.T) {
	withTestMetrics(t)
	captureSlog(t)
	first, second := make(chan routeEvent), make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: first}, {events: second}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	w.minInterval = 300 * time.Millisecond
	startRouteWatcher(t, w)

	waitForCondition(t, "the first subscribe", func() bool { return script.callCount() == 1 })
	closedAt := time.Now()
	close(first)
	waitForCondition(t, "the trigger on recovery", func() bool { return triggers.count() == 1 })

	sendRouteEvent(t, second, ownedKernelDelete)
	waitForCondition(t, "the trigger for drift on the new subscription", func() bool { return triggers.count() == 2 })

	// The recovery trigger came no earlier than backoffMin after the close,
	// and the drift trigger has to keep minInterval from it.
	if waited := triggers.times()[1].Sub(closedAt); waited < w.backoffMin+w.minInterval {
		t.Errorf("drift trigger came %v after the first subscription closed, want at least %v: the backoff plus the rate limit since the recovery trigger",
			waited, w.backoffMin+w.minInterval)
	}
}

func TestRouteWatcherLogsOneOutageAndTriggersOnRecovery(t *testing.T) {
	withTestMetrics(t)
	logs := captureSlog(t)
	script := &subscribeScript{results: []subscribeResult{
		{err: errors.New("netlink: permission denied")},
		{err: errors.New("netlink: permission denied")},
		{events: make(chan routeEvent)},
	}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	stop := startRouteWatcher(t, w)

	waitForCondition(t, "the trigger on recovery", func() bool { return triggers.count() > 0 })
	time.Sleep(2 * w.backoffMax)
	stop()

	if got := triggers.count(); got != 1 {
		t.Errorf("triggers = %d, want 1 on recovery", got)
	}
	if got := script.callCount(); got != 3 {
		t.Errorf("subscribe calls = %d, want 3", got)
	}
	if got := strings.Count(logs.String(), "route watch unavailable"); got != 1 {
		t.Errorf("outage warnings = %d, want 1 for one outage:\n%s", got, logs)
	}
	if !strings.Contains(logs.String(), "netlink: permission denied") {
		t.Errorf("the outage warning does not carry the subscribe error:\n%s", logs)
	}
	if got := strings.Count(logs.String(), "route watch is back"); got != 1 {
		t.Errorf("recovery lines = %d, want 1:\n%s", got, logs)
	}
}

func TestRouteWatcherResubscribesAfterClosedChannel(t *testing.T) {
	m := withTestMetrics(t)
	logs := captureSlog(t)
	first, second := make(chan routeEvent), make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: first}, {events: second}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	stop := startRouteWatcher(t, w)

	waitForCondition(t, "the first subscribe", func() bool { return script.callCount() == 1 })
	closedAt := time.Now()
	close(first)

	// Deletions may have been missed while the socket was gone, so the
	// second subscribe triggers once by itself.
	waitForCondition(t, "the trigger on recovery", func() bool { return triggers.count() == 1 })
	if waited := triggers.times()[0].Sub(closedAt); waited < w.backoffMin {
		t.Errorf("resubscribed %v after the channel closed, want at least the backoff of %v", waited, w.backoffMin)
	}
	if got := script.callCount(); got != 2 {
		t.Fatalf("subscribe calls = %d, want 2", got)
	}

	sendRouteEvent(t, second, ownedKernelDelete)
	waitForCondition(t, "the trigger for drift on the new subscription", func() bool { return triggers.count() == 2 })
	stop()

	if got := counterValue(t, m, routeDriftMetric, "kind", "kernel"); got != 1 {
		t.Errorf("%s{kind=\"kernel\"} = %v, want 1", routeDriftMetric, got)
	}
	if got := strings.Count(logs.String(), "route watch unavailable"); got != 1 {
		t.Errorf("outage warnings = %d, want 1:\n%s", got, logs)
	}
	if !strings.Contains(logs.String(), errRouteSubscriptionClosed.Error()) {
		t.Errorf("the outage warning does not say the subscription closed:\n%s", logs)
	}
}

func TestRouteWatcherDoublesTheWaitBetweenFailedSubscribes(t *testing.T) {
	captureSlog(t)
	failed := errors.New("netlink: permission denied")
	script := &subscribeScript{results: []subscribeResult{
		{err: failed}, {err: failed}, {err: failed}, {err: failed}, {err: failed},
	}}
	w := newTestRouteWatcher(t, script, &triggerLog{})
	startRouteWatcher(t, w)

	waitForCondition(t, "the fifth subscribe", func() bool { return script.callCount() >= 5 })

	at := script.callTimes()
	for i, want := range []time.Duration{w.backoffMin, 2 * w.backoffMin, w.backoffMax, w.backoffMax} {
		if gap := at[i+1].Sub(at[i]); gap < want {
			t.Errorf("subscribe %d came %v after subscribe %d, want at least %v", i+2, gap, i+1, want)
		}
	}
}

// A subscription that fails right after it opened does not start the wait
// over. Otherwise a socket that opens and dies at once would be retried, and
// the agent reconciled by the recovery trigger, every backoffMin.
func TestRouteWatcherKeepsBackingOffAfterShortLivedSubscription(t *testing.T) {
	captureSlog(t)
	failed := errors.New("netlink: permission denied")
	shortLived := make(chan routeEvent)
	close(shortLived)
	script := &subscribeScript{results: []subscribeResult{{err: failed}, {err: failed}, {events: shortLived}}}
	w := newTestRouteWatcher(t, script, &triggerLog{})
	startRouteWatcher(t, w)

	waitForCondition(t, "the subscribe after the short-lived subscription", func() bool { return script.callCount() >= 4 })

	// Two failures grew the wait to 2*backoffMin. The next step is backoffMax.
	at := script.callTimes()
	if gap := at[3].Sub(at[2]); gap < w.backoffMax {
		t.Errorf("subscribed again %v after a subscription that closed at once, want at least the grown backoff of %v", gap, w.backoffMax)
	}
}

func TestNextBackoff(t *testing.T) {
	const lo, hi = time.Second, 30 * time.Second
	cases := []struct {
		name        string
		prev, lived time.Duration
		want        time.Duration
	}{
		{name: "the first wait is the minimum", want: lo},
		{name: "a failed subscribe doubles the wait", prev: lo, want: 2 * lo},
		{name: "the doubling stops at the maximum", prev: 16 * time.Second, want: hi},
		{name: "the maximum stays the maximum", prev: hi, want: hi},
		{name: "a short-lived subscription keeps doubling", prev: 2 * time.Second, lived: hi - time.Nanosecond, want: 4 * time.Second},
		{name: "a subscription that held for the maximum starts over", prev: hi, lived: hi, want: lo},
		{name: "a first subscription that held for the maximum starts at the minimum", lived: time.Hour, want: lo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextBackoff(tc.prev, tc.lived, lo, hi); got != tc.want {
				t.Errorf("nextBackoff(%v, %v, %v, %v) = %v, want %v", tc.prev, tc.lived, lo, hi, got, tc.want)
			}
		})
	}
}

func TestRouteWatcherFiresPendingTriggerWhenChannelCloses(t *testing.T) {
	withTestMetrics(t)
	events := make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: events}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	// Neither the debounce nor the resubscribe can produce a trigger inside
	// this test, so the one it sees is the pending one, fired by the close.
	w.debounce = time.Minute
	w.backoffMin = time.Minute
	w.backoffMax = time.Minute
	startRouteWatcher(t, w)

	sendRouteEvent(t, events, ownedKernelDelete)
	close(events)

	waitForCondition(t, "the pending trigger", func() bool { return triggers.count() == 1 })
}

func TestRouteWatcherStopsForGoodWhenUnsupported(t *testing.T) {
	script := &subscribeScript{results: []subscribeResult{{err: errRouteWatchUnsupported}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	startRouteWatcher(t, w)

	// The context is still alive: the goroutine has to return by itself.
	if !returnsWithin(2*time.Second, w.wait) {
		t.Fatal("wait() did not return after subscribe reported an unsupported platform")
	}
	if got := script.callCount(); got != 1 {
		t.Errorf("subscribe calls = %d, want 1", got)
	}
	if got := triggers.count(); got != 0 {
		t.Errorf("triggers = %d, want 0", got)
	}
}

func TestRouteWatcherShutdownDropsPendingTrigger(t *testing.T) {
	withTestMetrics(t)
	events := make(chan routeEvent)
	script := &subscribeScript{results: []subscribeResult{{events: events}}}
	triggers := &triggerLog{}
	w := newTestRouteWatcher(t, script, triggers)
	w.debounce = 50 * time.Millisecond
	stop := startRouteWatcher(t, w)

	sendRouteEvent(t, events, ownedKernelDelete)
	if !returnsWithin(2*time.Second, stop) {
		t.Fatal("wait() did not return after the context was cancelled")
	}

	time.Sleep(2 * w.debounce)
	if got := triggers.count(); got != 0 {
		t.Errorf("triggers = %d, want 0 after shutdown", got)
	}
}

func TestRouteWatcherNilReceiverIsNoOp(t *testing.T) {
	var w *routeWatcher
	owned := testRouteOwnership(t)
	ok := returnsWithin(2*time.Second, func() {
		w.setOwned(owned)
		w.start(context.Background())
		w.wait()
	})
	if !ok {
		t.Fatal("a method of a nil routeWatcher blocked")
	}
}

func TestRouteWatcherWaitReturnsWhenNeverStarted(t *testing.T) {
	w := newRouteWatcher(Config{}, func() {})
	if !returnsWithin(2*time.Second, w.wait) {
		t.Fatal("wait() blocked on a watcher that was never started")
	}
}

// A zero backoff would turn a failing subscribe into a busy loop, and a zero
// debounce or rate limit would reconcile once per event.
func TestNewRouteWatcherUsesProductionTimings(t *testing.T) {
	w := newRouteWatcher(Config{VethNexthop: testWatchNexthop}, func() {})

	if w.debounce != routeDriftDebounce || w.minInterval != routeDriftMinInterval {
		t.Errorf("debounce = %v, minInterval = %v, want %v and %v",
			w.debounce, w.minInterval, routeDriftDebounce, routeDriftMinInterval)
	}
	if w.backoffMin != routeWatchBackoffMin || w.backoffMax != routeWatchBackoffMax {
		t.Errorf("backoff = %v to %v, want %v to %v",
			w.backoffMin, w.backoffMax, routeWatchBackoffMin, routeWatchBackoffMax)
	}
	if w.vethNexthop != testWatchNexthop {
		t.Errorf("vethNexthop = %q, want %q", w.vethNexthop, testWatchNexthop)
	}
}
