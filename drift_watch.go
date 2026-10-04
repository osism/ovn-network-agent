package main

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// driftEvent is an event a drift watch reports. watchSubject names the watch
// in the log lines of its driftWatcher ("route", "OVS flow"). It is a method
// of the event type rather than a field of the watcher, so every message is a
// constant of its watch: gosec's log injection check treats a message read
// from the watcher as input.
type driftEvent interface {
	watchSubject() string
}

// errDriftWatchUnsupported is what a subscribe returns on a platform where its
// watch cannot work. The watcher stops for good on it.
var errDriftWatchUnsupported = errors.New("watch is not supported on this platform")

// driftWatcher is the loop both watches run: it subscribes to a source of
// events, turns the events that are drift into a debounced, rate-limited
// reconcile trigger, and subscribes again with a backoff when the
// subscription fails. What an event is and when it is drift is up to the
// watch that embeds it (route_watch.go, flow_watch.go).
//
// The fields are set once, before start, and only read afterwards.
type driftWatcher[E driftEvent] struct {
	// kinds lists the drift kinds in the order the trigger log line lists
	// their counts.
	kinds []string
	// closedErr is the error the outage warning carries when a
	// subscription ended because its event channel was closed.
	closedErr error
	// subscribe opens one subscription. The watcher closes done when it is
	// finished with that subscription.
	subscribe func(done <-chan struct{}) (<-chan E, error)
	// classify reports whether ev is drift, and of which kind.
	classify func(ev E) (kind string, drift bool)
	// onDrift is called once for every drift event, before it counts
	// towards the trigger.
	onDrift func(ev E, kind string)
	trigger func()

	debounce    time.Duration
	minInterval time.Duration
	backoffMin  time.Duration
	backoffMax  time.Duration

	// stopped is closed when the goroutine has returned. Nil until start.
	stopped chan struct{}
}

// start runs the watcher on its own goroutine until ctx is done.
func (w *driftWatcher[E]) start(ctx context.Context) {
	w.stopped = make(chan struct{})
	go func() {
		defer close(w.stopped)
		w.run(ctx)
	}()
}

// wait blocks until the goroutine has returned. It returns at once when start
// was never called.
func (w *driftWatcher[E]) wait() {
	if w.stopped == nil {
		return
	}
	<-w.stopped
}

// run subscribes, handles the events of that subscription, and subscribes
// again with a backoff when the subscription fails. A watcher that cannot
// subscribe leaves the agent where it is without one: on the periodic
// reconcile.
func (w *driftWatcher[E]) run(ctx context.Context) {
	var zero E
	subject := zero.watchSubject()
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
		if errors.Is(err, errDriftWatchUnsupported) {
			close(done)
			slog.Debug(subject + " watch is not supported on this platform")
			return
		}
		// lived is how long this subscription held, 0 when the subscribe
		// failed.
		var lived time.Duration
		if err == nil {
			if outage {
				// Changes during the outage were not seen.
				outage = false
				slog.Info(subject + " watch is back")
				lastTrigger = time.Now()
				w.trigger()
			}
			subscribedAt := time.Now()
			lastTrigger = w.consume(ctx, events, lastTrigger)
			lived = time.Since(subscribedAt)
			err = w.closedErr
		}
		close(done)
		if ctx.Err() != nil {
			return
		}

		if !outage {
			outage = true
			slog.Warn(subject+" watch unavailable, drift is repaired by the periodic reconcile until it is back", "error", err)
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
func (w *driftWatcher[E]) consume(ctx context.Context, events <-chan E, lastTrigger time.Time) time.Time {
	var zero E
	subject := zero.watchSubject()
	var (
		timer  *time.Timer
		fire   <-chan time.Time
		counts = make(map[string]int, len(w.kinds))
	)
	fireTrigger := func() {
		timer, fire = nil, nil
		if ctx.Err() != nil {
			return
		}
		args := make([]any, 0, 2*len(w.kinds))
		for _, kind := range w.kinds {
			args = append(args, kind, counts[kind])
		}
		slog.Info(subject+" drift detected, reconciling", args...)
		clear(counts)
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
				// The subscription failed, so later changes go unseen. A
				// pending trigger does not wait for its timer.
				if timer != nil {
					timer.Stop()
					fireTrigger()
				}
				return lastTrigger
			}
			kind, drift := w.classify(ev)
			if !drift {
				continue
			}
			w.onDrift(ev, kind)
			counts[kind]++
			if timer == nil {
				timer = time.NewTimer(max(w.debounce, w.minInterval-time.Since(lastTrigger)))
				fire = timer.C
			}

		case <-fire:
			fireTrigger()
		}
	}
}
