package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// subscribeResult is what one call of a scripted subscribe returns.
type subscribeResult[E driftEvent] struct {
	events chan E
	err    error
}

// subscribeScript stands in for the subscription of a watch. Each call
// returns the next scripted result. Once the script is used up a call returns
// a channel that never delivers, so the watcher idles on it.
type subscribeScript[E driftEvent] struct {
	mu      sync.Mutex
	results []subscribeResult[E]
	at      []time.Time // when each call came in
}

func (s *subscribeScript[E]) subscribe(<-chan struct{}) (<-chan E, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at = append(s.at, time.Now())
	if len(s.results) == 0 {
		return make(chan E), nil
	}
	r := s.results[0]
	s.results = s.results[1:]
	return r.events, r.err
}

func (s *subscribeScript[E]) callTimes() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.at...)
}

func (s *subscribeScript[E]) callCount() int { return len(s.callTimes()) }

// startWatcher starts w and returns the function that stops it and waits for
// it. The function also runs at the end of the test. Call withTestMetrics and
// captureSlog before this, so their cleanups run after the goroutine is gone.
func startWatcher(t *testing.T, w interface {
	start(ctx context.Context)
	wait()
}) (stop func()) {
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

// sendEvent hands ev to the watcher. The channel is unbuffered, so the watcher
// has taken the event when this returns.
func sendEvent[E driftEvent](t *testing.T, events chan<- E, ev E) {
	t.Helper()
	select {
	case events <- ev:
	case <-time.After(2 * time.Second):
		t.Fatalf("the watcher did not take the event %+v", ev)
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
