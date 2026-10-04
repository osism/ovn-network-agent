package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The FRR watch makes the agent reconcile as soon as a restarted FRR has
// settled. The agent writes its /32 statics and its prefix-list entries into
// FRR's running configuration and never saves them: a "write memory" would
// overwrite the operator's frr.conf. A restarted FRR therefore comes back
// without them, and the route watch's reconcile while FRR stops runs against
// daemons that are about to exit. A bgpd-only restart changes no kernel route,
// so no other watch sees it at all.
//
// FRR offers no event stream, so the watch polls `vtysh -c "show modules"`,
// whose answer carries the process ID of every daemon that answers, and treats
// a settled change of those process IDs as a restart. A failed poll is the
// expected state while FRR restarts, which is why this watch runs its own loop
// rather than the driftWatcher of drift_watch.go: that one consumes an event
// subscription and backs off when it fails.

const (
	// frrWatchInterval is how often the watch polls the FRR daemons.
	frrWatchInterval = time.Second

	// frrWatchPollTimeout bounds one vtysh call.
	frrWatchPollTimeout = 5 * time.Second

	// frrRestartFollowUp is how long after a restart the watch triggers a
	// second reconcile. FRR applies its saved configuration with "vtysh -b"
	// only after the daemons have started, and a saved prefix-list line can
	// overwrite an entry the first reconcile wrote.
	frrRestartFollowUp = 5 * time.Second

	// frrWatchOutageWarn is how long the polls have to fail in a row before
	// the watch warns that it cannot see a restart.
	frrWatchOutageWarn = 30 * time.Second
)

// frrIncarnation maps the name of every FRR daemon that answered a poll to its
// process ID.
type frrIncarnation map[string]int

// errFRRModulesUnparsed is what parseFRRModules returns for an answer that
// names no daemon with a process ID. The watch counts it as a failed poll, so
// an FRR that stops printing the pid line warns instead of never triggering.
var errFRRModulesUnparsed = errors.New("show modules answer names no daemon with a process ID")

// parseFRRModules reads the process ID of every daemon out of the answer to
// "show modules". vtysh prints a "Module information for <daemon>:" header
// for each daemon that answers, and FRR 8.0 and later print a "pid: <n>" line
// in each daemon's section. Every other line is ignored, vtysh's own warnings
// included, and so is a pid line that is not a positive number.
func parseFRRModules(out []byte) (frrIncarnation, error) {
	inc := frrIncarnation{}
	daemon := ""
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Module information for "); ok {
			// A header without its colon still ends the previous
			// section, and its pid line is credited to no daemon.
			daemon = ""
			if name, ok := strings.CutSuffix(rest, ":"); ok {
				daemon = name
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "pid:"); ok && daemon != "" {
			if pid, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil && pid > 0 {
				inc[daemon] = pid
			}
			daemon = ""
		}
	}
	if len(inc) == 0 {
		return nil, errFRRModulesUnparsed
	}
	return inc, nil
}

// showModules is the watch's poll. It runs `vtysh -c "show modules"` and gives
// up after frrWatchPollTimeout. vtysh is resolved on PATH, like the agent's
// other vtysh calls, so a wrapper in front of a containerized FRR is polled
// the same way.
func showModules(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, frrWatchPollTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "vtysh", "-c", "show modules")
	// Bounds the wait for the output pipe, which a descendant of a killed
	// wrapper may hold open.
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("vtysh show modules: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// frrStep is what one poll asks the watcher to do.
type frrStep struct {
	// restart is non-nil when FRR restarted. It lists, sorted, the daemons
	// whose process ID changed, appeared or disappeared, or every daemon
	// when FRR came up after the agent started without it.
	restart []string
	// followUp asks for the second reconcile after a restart.
	followUp bool
	// warn asks for the warning that the polls have failed for
	// frrWatchOutageWarn.
	warn bool
	// back reports the first successful poll after that warning.
	back bool
}

// frrWatchState is the watch's decision logic. It is fed one poll at a time
// and holds no goroutine and no timer, so every timing rule is tested step by
// step.
//
// An answer is settled when it equals the previous one. That waits out the
// daemons starting one by one, and puts the first reconcile at least one poll
// after the last daemon appeared, which leaves room for FRR's "vtysh -b".
type frrWatchState struct {
	// known is the incarnation the last trigger stood for, the startup
	// reconcile included, nil until FRR answered.
	known frrIncarnation
	// prev is the previous answer, nil after a failed poll.
	prev frrIncarnation
	// sawFailure records a failed poll before the first settled answer, the
	// poll before the startup reconcile included: FRR was not up when the
	// agent started, so its first settled answer is a restart rather than
	// the baseline.
	sawFailure bool
	// failingSince is when the current run of failed polls began, zero
	// while the polls succeed.
	failingSince time.Time
	// warned is set from the outage warning until a poll succeeds.
	warned bool
	// followUpAt is when the follow-up reconcile is due, zero when none is
	// pending.
	followUpAt time.Time

	// followUpDelay is how long after a restart the follow-up is due:
	// frrRestartFollowUp in production, shorter in the watcher tests.
	followUpDelay time.Duration
	// outageWarnAfter is how long the polls have to fail in a row before the
	// warning: frrWatchOutageWarn in production, shorter in the watcher
	// tests.
	outageWarnAfter time.Duration
}

// prime takes the answer of the poll before the startup reconcile, or the
// error it failed with, as the incarnation that reconcile runs against. The
// answer need not be settled: every settled answer that differs from it is a
// restart.
func (s *frrWatchState) prime(inc frrIncarnation, err error) {
	if err != nil {
		s.sawFailure = true
		return
	}
	s.known = inc
}

// step takes the answer of one poll, or the error it failed with, at now.
func (s *frrWatchState) step(inc frrIncarnation, err error, now time.Time) frrStep {
	var st frrStep
	if err != nil {
		s.prev = nil
		if s.known == nil {
			s.sawFailure = true
		}
		if s.failingSince.IsZero() {
			s.failingSince = now
		}
		if !s.warned && now.Sub(s.failingSince) >= s.outageWarnAfter {
			s.warned = true
			st.warn = true
		}
	} else {
		st.back = s.warned
		s.warned = false
		s.failingSince = time.Time{}
		settled := maps.Equal(inc, s.prev)
		s.prev = inc
		switch {
		case !settled:
		case s.known == nil:
			// After a failed poll this is a restart. Without one, the state
			// was not primed and this is the baseline.
			if s.sawFailure {
				st.restart = changedDaemons(nil, inc)
			}
			s.known = inc
		case !maps.Equal(inc, s.known):
			st.restart = changedDaemons(s.known, inc)
			s.known = inc
		}
		if st.restart != nil {
			s.followUpAt = now.Add(s.followUpDelay)
		}
	}
	if !s.followUpAt.IsZero() && !now.Before(s.followUpAt) {
		st.followUp = true
		s.followUpAt = time.Time{}
	}
	return st
}

// changedDaemons returns, sorted, the daemons whose process ID differs
// between old and cur, those present in only one of them included. With old
// nil it returns every daemon of cur.
func changedDaemons(old, cur frrIncarnation) []string {
	var names []string
	for name, pid := range cur {
		if old[name] != pid {
			names = append(names, name)
		}
	}
	for name := range old {
		if _, ok := cur[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// frrWatcher polls the FRR daemons on its own goroutine and triggers a
// reconcile when FRR restarted, and once more frrRestartFollowUp later. It
// never touches the RouteManager or the agent's configuration.
//
// prime, start and wait are no-ops on a nil receiver, so an agent without a
// watcher (frr_watch off, dry-run, port-forward-only mode) calls them
// unguarded.
type frrWatcher struct {
	poll     func(context.Context) ([]byte, error)
	trigger  func()
	interval time.Duration
	state    frrWatchState

	// stopped is closed when the goroutine has returned. Nil until start.
	stopped chan struct{}
}

// newFRRWatcher builds a watcher that calls trigger when FRR restarted.
func newFRRWatcher(trigger func()) *frrWatcher {
	return &frrWatcher{
		poll:     showModules,
		trigger:  trigger,
		interval: frrWatchInterval,
		state:    frrWatchState{followUpDelay: frrRestartFollowUp, outageWarnAfter: frrWatchOutageWarn},
	}
}

// prime polls once before the startup reconcile, so an FRR that comes up or
// restarts while that reconcile runs is a restart rather than the baseline.
// It runs on the caller's goroutine, before start.
func (w *frrWatcher) prime(ctx context.Context) {
	if w == nil {
		return
	}
	inc, err := w.read(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		slog.Debug("FRR watch poll failed", "error", err)
	}
	w.state.prime(inc, err)
}

// start runs the watcher on its own goroutine until ctx is done.
func (w *frrWatcher) start(ctx context.Context) {
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
func (w *frrWatcher) wait() {
	if w == nil || w.stopped == nil {
		return
	}
	<-w.stopped
}

// run polls once at start and then on every tick until ctx is done. A tick
// that falls while a poll runs is dropped by the ticker.
func (w *frrWatcher) run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// read runs one poll and parses its answer.
func (w *frrWatcher) read(ctx context.Context) (frrIncarnation, error) {
	out, err := w.poll(ctx)
	if err != nil {
		return nil, err
	}
	return parseFRRModules(out)
}

// pollOnce runs one poll and acts on what the state decides about it. A poll
// that ends because ctx is done is not acted on.
func (w *frrWatcher) pollOnce(ctx context.Context) {
	inc, err := w.read(ctx)
	if ctx.Err() != nil {
		return
	}
	st := w.state.step(inc, err, time.Now())

	if err != nil {
		slog.Debug("FRR watch poll failed", "error", err)
	}
	if st.warn {
		slog.Warn("FRR watch cannot read the FRR daemons, an FRR restart waits for the periodic reconcile until it can", "error", err)
	}
	if st.back {
		slog.Info("FRR watch can read the FRR daemons again")
	}
	if st.restart != nil {
		recordFRRRestart()
		slog.Info("FRR restart detected, reconciling", "daemons", strings.Join(st.restart, ","))
		w.trigger()
	}
	if st.followUp {
		slog.Info("reconciling again after the FRR restart")
		w.trigger()
	}
}
