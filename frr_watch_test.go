package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The sections of the answer vtysh -c "show modules" gave against FRR 8.4.4.
const (
	frrModulesZebra844 = `Module information for zebra:
Module Name  Version                   Description

libfrr       8.4.4                     libfrr core module
zebra        8.4.4                     zebra daemon
pid: 825

`
	frrModulesBGPD844 = `Module information for bgpd:
Module Name  Version                   Description

libfrr       8.4.4                     libfrr core module
bgpd         8.4.4                     bgpd daemon
pid: 830

`
	frrModulesWatchfrr844 = `Module information for watchfrr:
Module Name  Version                   Description

libfrr       8.4.4                     libfrr core module
watchfrr     8.4.4                     watchfrr daemon
pid: 812

`
	frrModulesStaticd844 = `Module information for staticd:
Module Name  Version                   Description

libfrr       8.4.4                     libfrr core module
staticd      8.4.4                     staticd daemon
pid: 837
`
	frrModulesAnswer844 = frrModulesZebra844 + frrModulesBGPD844 + frrModulesWatchfrr844 + frrModulesStaticd844

	frrRestartsMetric = "ovn_network_agent_frr_restarts_total"
)

// The incarnations of the step tests: B is A after a restart of zebra and
// bgpd, C is A after a restart of all three daemons, D is C restarted again.
var (
	frrA = frrIncarnation{"zebra": 825, "bgpd": 830, "staticd": 837}
	frrB = frrIncarnation{"zebra": 925, "bgpd": 930, "staticd": 837}
	frrC = frrIncarnation{"zebra": 925, "bgpd": 930, "staticd": 937}
	frrD = frrIncarnation{"zebra": 1025, "bgpd": 1030, "staticd": 1037}
)

var errTestVtyshDown = errors.New("test: vtysh show modules: exit status 1")

// modulesAnswer renders inc as a "show modules" answer, daemons sorted.
func modulesAnswer(inc frrIncarnation) []byte {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(inc)) {
		fmt.Fprintf(&b, "Module information for %s:\nModule Name  Version  Description\n\nlibfrr  8.4.4  libfrr core module\npid: %d\n\n", name, inc[name])
	}
	return []byte(b.String())
}

func TestParseFRRModules(t *testing.T) {
	cases := []struct {
		name    string
		out     []byte
		want    frrIncarnation
		wantErr error
	}{
		{
			name: "FRR 8.4.4 answer",
			out:  []byte(frrModulesAnswer844),
			want: frrIncarnation{"zebra": 825, "bgpd": 830, "watchfrr": 812, "staticd": 837},
		},
		{
			name: "bgpd down",
			out:  []byte(frrModulesZebra844 + frrModulesWatchfrr844 + frrModulesStaticd844),
			want: frrIncarnation{"zebra": 825, "watchfrr": 812, "staticd": 837},
		},
		{
			name: "vtysh warnings around the sections",
			out:  []byte("% Warning: some daemon is not running\n" + frrModulesZebra844 + "pid: 999\n"),
			want: frrIncarnation{"zebra": 825},
		},
		{
			name: "pid lines that are not a positive number",
			out: []byte(strings.Replace(frrModulesZebra844, "pid: 825", "pid: abc", 1) +
				strings.Replace(frrModulesBGPD844, "pid: 830", "pid: 0", 1) +
				frrModulesStaticd844),
			want: frrIncarnation{"staticd": 837},
		},
		{
			name: "a header without its colon",
			out: []byte(strings.Replace(frrModulesZebra844, "pid: 825\n", "", 1) +
				"Module information for bgpd\npid: 830\n\n" + frrModulesStaticd844),
			want: frrIncarnation{"staticd": 837},
		},
		{name: "nil", out: nil, wantErr: errFRRModulesUnparsed},
		{name: "empty", out: []byte{}, wantErr: errFRRModulesUnparsed},
		{
			name: "headers without a pid line",
			out: []byte(strings.Replace(frrModulesZebra844, "pid: 825\n", "", 1) +
				strings.Replace(frrModulesBGPD844, "pid: 830\n", "", 1)),
			wantErr: errFRRModulesUnparsed,
		},
		{name: "no daemon reachable", out: []byte("Exiting: failed to connect to any daemons.\n"), wantErr: errFRRModulesUnparsed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFRRModules(tc.out)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("parseFRRModules() error = %v, want %v", err, tc.wantErr)
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("parseFRRModules() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The parser reads another process's output. Whatever it is fed, it must not
// panic, and an answer it accepts names at least one daemon, each with a
// positive process ID.
func FuzzParseFRRModules(f *testing.F) {
	for _, seed := range []string{
		frrModulesAnswer844, frrModulesZebra844, "", "pid: 825\n",
		"Module information for zebra:\npid: abc\n", "Module information for :\npid: 1\n",
		"Exiting: failed to connect to any daemons.\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, out []byte) {
		inc, err := parseFRRModules(out)
		if err != nil {
			if !errors.Is(err, errFRRModulesUnparsed) || inc != nil {
				t.Errorf("parseFRRModules(%q) = %v, %v: want a nil map and errFRRModulesUnparsed", out, inc, err)
			}
			return
		}
		if len(inc) == 0 {
			t.Errorf("parseFRRModules(%q) accepted an answer without a daemon", out)
		}
		for name, pid := range inc {
			if pid <= 0 {
				t.Errorf("parseFRRModules(%q) recorded pid %d for %q", out, pid, name)
			}
		}
	})
}

func TestShowModulesWithoutVtysh(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := showModules(context.Background())
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("showModules() error = %v, want exec.ErrNotFound", err)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "vtysh show modules:") {
		t.Errorf("showModules() error = %v, want the prefix %q", err, "vtysh show modules:")
	}
}

// A vtysh that hangs, as one in front of a wedged daemon can, must not hold
// the watch beyond the timeout plus the pipe's WaitDelay.
func TestShowModulesGivesUpAtTheTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vtysh"), []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatalf("write fake vtysh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The earlier deadline wins over frrWatchPollTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := showModules(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("showModules() returned after %v, want within 2s", elapsed)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "vtysh show modules:") {
		t.Errorf("showModules() error = %v, want one with the prefix %q", err, "vtysh show modules:")
	}
}

// frrPoll is one poll of a step test: an answer, or the error it failed with.
type frrPoll struct {
	inc frrIncarnation
	err error
}

// answered is a poll that returned inc, failedPoll one that failed.
func answered(inc frrIncarnation) frrPoll { return frrPoll{inc: inc} }

var failedPoll = frrPoll{err: errTestVtyshDown}

func newTestFRRWatchState() *frrWatchState {
	return &frrWatchState{followUpDelay: 5 * time.Second, outageWarnAfter: 30 * time.Second}
}

func TestFRRWatchStepRestarts(t *testing.T) {
	withOSPFD := maps.Clone(frrA)
	withOSPFD["ospfd"] = 840

	cases := []struct {
		name string
		// prime is the poll before the startup reconcile, nil for a state
		// that was not primed.
		prime *frrPoll
		polls []frrPoll
		// restartAt is the 1-based poll that reports the restart, 0 for none.
		restartAt   int
		wantRestart []string
	}{
		{name: "unchanged", polls: []frrPoll{answered(frrA), answered(frrA), answered(frrA)}},
		{name: "first settled answer is the baseline", polls: []frrPoll{answered(frrA), answered(frrA)}},
		{
			name:  "a failed poll is no restart",
			polls: []frrPoll{answered(frrA), answered(frrA), failedPoll, failedPoll, answered(frrA), answered(frrA)},
		},
		{
			name:  "an unparsable answer is no restart",
			polls: []frrPoll{answered(frrA), answered(frrA), {err: errFRRModulesUnparsed}, answered(frrA), answered(frrA)},
		},
		{name: "unchanged since the prime", prime: &frrPoll{inc: frrA}, polls: []frrPoll{answered(frrA), answered(frrA)}},
		{
			name:  "FRR came up while the startup reconcile ran",
			prime: &frrPoll{err: errTestVtyshDown}, polls: []frrPoll{answered(frrA), answered(frrA)},
			restartAt: 2, wantRestart: []string{"bgpd", "staticd", "zebra"},
		},
		{
			name:  "FRR finished starting while the startup reconcile ran",
			prime: &frrPoll{inc: frrIncarnation{"zebra": 825}}, polls: []frrPoll{answered(frrA), answered(frrA)},
			restartAt: 2, wantRestart: []string{"bgpd", "staticd"},
		},
		{
			name:  "FRR restarted while the startup reconcile ran",
			prime: &frrPoll{inc: frrA}, polls: []frrPoll{answered(frrB), answered(frrB)},
			restartAt: 2, wantRestart: []string{"bgpd", "zebra"},
		},
		{
			name:      "zebra and bgpd restarted",
			polls:     []frrPoll{answered(frrA), answered(frrA), answered(frrB), answered(frrB)},
			restartAt: 4, wantRestart: []string{"bgpd", "zebra"},
		},
		{
			name:      "restart through an outage, daemons starting one by one",
			polls:     []frrPoll{answered(frrA), answered(frrA), failedPoll, failedPoll, answered(frrIncarnation{"zebra": 925}), answered(frrC), answered(frrC)},
			restartAt: 7, wantRestart: []string{"bgpd", "staticd", "zebra"},
		},
		{
			name:      "FRR came up after the agent started",
			polls:     []frrPoll{failedPoll, answered(frrA), answered(frrA)},
			restartAt: 3, wantRestart: []string{"bgpd", "staticd", "zebra"},
		},
		{name: "never settled", polls: []frrPoll{answered(frrA), answered(frrB), answered(frrC), answered(frrD)}},
		{
			name:      "a daemon removed for good",
			polls:     []frrPoll{answered(withOSPFD), answered(withOSPFD), answered(frrA), answered(frrA)},
			restartAt: 4, wantRestart: []string{"ospfd"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestFRRWatchState()
			if tc.prime != nil {
				s.prime(tc.prime.inc, tc.prime.err)
			}
			t0 := time.Unix(1_000_000, 0)
			for i, p := range tc.polls {
				st := s.step(p.inc, p.err, t0.Add(time.Duration(i)*time.Second))
				n := i + 1
				if n == tc.restartAt {
					if !slices.Equal(st.restart, tc.wantRestart) || st.restart == nil {
						t.Errorf("poll %d: restart = %q, want %q", n, st.restart, tc.wantRestart)
					}
				} else if st.restart != nil {
					t.Errorf("poll %d: restart = %q, want none", n, st.restart)
				}
				if st.followUp || st.warn || st.back {
					t.Errorf("poll %d: step = %+v, want no follow-up, warning or recovery", n, st)
				}
			}
		})
	}
}

func TestFRRWatchStepFollowUp(t *testing.T) {
	// restartAt feeds A, A, B, B so the last poll, at t, reports a restart.
	restartAt := func(t *testing.T, s *frrWatchState, at time.Time) {
		t.Helper()
		for i, inc := range []frrIncarnation{frrA, frrA, frrB} {
			s.step(inc, nil, at.Add(time.Duration(i-3)*time.Second))
		}
		if st := s.step(frrB, nil, at); st.restart == nil {
			t.Fatalf("the setup did not report a restart at %v", at)
		}
	}
	at := time.Unix(1_000_000, 0)

	t.Run("due once, 5s after the restart", func(t *testing.T) {
		s := newTestFRRWatchState()
		restartAt(t, s, at)
		for _, tc := range []struct {
			after time.Duration
			want  bool
		}{
			{4900 * time.Millisecond, false},
			{5 * time.Second, true},
			{6 * time.Second, false},
			{12 * time.Second, false},
		} {
			if st := s.step(frrB, nil, at.Add(tc.after)); st.followUp != tc.want {
				t.Errorf("t+%v: followUp = %v, want %v", tc.after, st.followUp, tc.want)
			}
		}
	})

	t.Run("due through failed polls", func(t *testing.T) {
		s := newTestFRRWatchState()
		restartAt(t, s, at)
		if st := s.step(nil, errTestVtyshDown, at.Add(5*time.Second)); !st.followUp {
			t.Error("t+5s: a failed poll did not report the due follow-up")
		}
	})

	t.Run("a second restart moves it", func(t *testing.T) {
		s := newTestFRRWatchState()
		restartAt(t, s, at)
		s.step(frrC, nil, at.Add(2*time.Second))
		if st := s.step(frrC, nil, at.Add(3*time.Second)); st.restart == nil {
			t.Fatal("t+3s: the second restart was not reported")
		}
		for _, tc := range []struct {
			after time.Duration
			want  bool
		}{
			{5 * time.Second, false},
			{7900 * time.Millisecond, false},
			{8 * time.Second, true},
			{9 * time.Second, false},
		} {
			if st := s.step(frrC, nil, at.Add(tc.after)); st.followUp != tc.want {
				t.Errorf("t+%v: followUp = %v, want %v", tc.after, st.followUp, tc.want)
			}
		}
	})
}

func TestFRRWatchStepOutageWarning(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)

	for _, failure := range []error{errTestVtyshDown, errFRRModulesUnparsed} {
		t.Run(failure.Error(), func(t *testing.T) {
			s := newTestFRRWatchState()
			s.step(frrA, nil, t0.Add(-2*time.Second))
			s.step(frrA, nil, t0.Add(-time.Second))
			for _, tc := range []struct {
				after time.Duration
				want  bool
			}{
				{0, false},
				{29 * time.Second, false},
				{30 * time.Second, true},
				{31 * time.Second, false},
				{90 * time.Second, false},
			} {
				if st := s.step(nil, failure, t0.Add(tc.after)); st.warn != tc.want {
					t.Errorf("t0+%v: warn = %v, want %v", tc.after, st.warn, tc.want)
				}
			}
			if st := s.step(frrA, nil, t0.Add(91*time.Second)); !st.back {
				t.Error("the first successful poll after the warning did not report back")
			}
			if st := s.step(frrA, nil, t0.Add(92*time.Second)); st.back {
				t.Error("a second successful poll reported back again")
			}
		})
	}

	t.Run("an outage shorter than the warning", func(t *testing.T) {
		s := newTestFRRWatchState()
		for i := range 29 {
			if st := s.step(nil, errTestVtyshDown, t0.Add(time.Duration(i)*time.Second)); st.warn {
				t.Fatalf("t0+%ds: warned before 30s", i)
			}
		}
		if st := s.step(frrA, nil, t0.Add(29*time.Second)); st.back {
			t.Error("a successful poll before the warning reported back")
		}
		// The next outage counts from its own first failure.
		if st := s.step(nil, errTestVtyshDown, t0.Add(40*time.Second)); st.warn {
			t.Error("the next outage inherited the time of the previous one")
		}
	})
}

func TestFRRWatcherTriggersOnARestartAndOnceMore(t *testing.T) {
	m := withTestMetrics(t)
	logs := captureSlog(t)

	var triggers atomic.Int32
	w := newFRRWatcher(func() { triggers.Add(1) })
	answers := [][]byte{modulesAnswer(frrA), modulesAnswer(frrA), modulesAnswer(frrB)}
	polls := 0
	w.poll = func(context.Context) ([]byte, error) {
		answer := answers[min(polls, len(answers)-1)]
		polls++
		return answer, nil
	}
	w.interval = time.Millisecond
	w.state.followUpDelay = 20 * time.Millisecond
	stop := startWatcher(t, w)

	waitForCondition(t, "the restart and its follow-up", func() bool { return triggers.Load() >= 2 })
	time.Sleep(60 * time.Millisecond)
	stop()

	if got := triggers.Load(); got != 2 {
		t.Errorf("triggers = %d, want 2: the restart and its follow-up", got)
	}
	if got := plainCounterValue(t, m, frrRestartsMetric); got != 1 {
		t.Errorf("%s = %v, want 1", frrRestartsMetric, got)
	}
	out := logs.String()
	for _, want := range []string{
		`msg="FRR restart detected, reconciling" daemons=bgpd,zebra`,
		`msg="reconciling again after the FRR restart"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not contain %q:\n%s", want, out)
		}
	}
}

// A watcher that cannot read FRR stays quiet and never triggers: the periodic
// reconcile is the repair until it can.
func TestFRRWatcherDoesNotTriggerWhileThePollFails(t *testing.T) {
	m := withTestMetrics(t)
	logs := captureSlog(t)

	var triggers, polls atomic.Int32
	w := newFRRWatcher(func() { triggers.Add(1) })
	w.poll = func(context.Context) ([]byte, error) {
		polls.Add(1)
		return []byte("Exiting: failed to connect to any daemons."), errTestVtyshDown
	}
	w.interval = time.Millisecond
	stop := startWatcher(t, w)

	waitForCondition(t, "the watcher to poll again", func() bool { return polls.Load() >= 5 })
	stop()

	if got := triggers.Load(); got != 0 {
		t.Errorf("triggers = %d, want 0", got)
	}
	if got := plainCounterValue(t, m, frrRestartsMetric); got != 0 {
		t.Errorf("%s = %v, want 0", frrRestartsMetric, got)
	}
	if out := logs.String(); out != "" {
		t.Errorf("failed polls inside the outage window logged above debug:\n%s", out)
	}
}

// The warning and the recovery line are an operator's only sign that the
// watch was blind, whether vtysh failed or answered without the pid lines
// (FRR before 8.0).
func TestFRRWatcherWarnsOnceWhileBlindAndReportsBack(t *testing.T) {
	const (
		warning  = "FRR watch cannot read the FRR daemons"
		recovery = "FRR watch can read the FRR daemons again"
	)
	cases := []struct {
		name    string
		fail    func() ([]byte, error)
		wantErr error
	}{
		{
			name:    "vtysh fails",
			fail:    func() ([]byte, error) { return nil, errTestVtyshDown },
			wantErr: errTestVtyshDown,
		},
		{
			name:    "an answer without a pid line",
			fail:    func() ([]byte, error) { return []byte("Module information for zebra:\n"), nil },
			wantErr: errFRRModulesUnparsed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTestMetrics(t)
			logs := captureSlog(t)

			// The polls fail for well over outageWarnAfter, counted from the
			// first poll, then succeed. Only the watcher's goroutine polls.
			var blindUntil time.Time
			var healthyPolls atomic.Int32
			w := newFRRWatcher(func() {})
			w.poll = func(context.Context) ([]byte, error) {
				if blindUntil.IsZero() {
					blindUntil = time.Now().Add(100 * time.Millisecond)
				}
				if time.Now().Before(blindUntil) {
					return tc.fail()
				}
				healthyPolls.Add(1)
				return modulesAnswer(frrA), nil
			}
			w.interval = time.Millisecond
			w.state.outageWarnAfter = 20 * time.Millisecond
			stop := startWatcher(t, w)

			waitForCondition(t, "polls after the outage", func() bool { return healthyPolls.Load() >= 5 })
			stop()

			out := logs.String()
			if got := strings.Count(out, warning); got != 1 {
				t.Errorf("warnings = %d, want 1 for one outage:\n%s", got, out)
			}
			if !strings.Contains(out, tc.wantErr.Error()) {
				t.Errorf("the warning does not carry %q:\n%s", tc.wantErr, out)
			}
			if got := strings.Count(out, recovery); got != 1 {
				t.Errorf("recoveries = %d, want 1 for one outage:\n%s", got, out)
			}
			if strings.Index(out, recovery) < strings.Index(out, warning) {
				t.Errorf("the recovery was logged before the warning:\n%s", out)
			}
		})
	}
}

func TestFRRWatcherStopsWhileAPollBlocks(t *testing.T) {
	var entered atomic.Bool
	var triggers atomic.Int32
	w := newFRRWatcher(func() { triggers.Add(1) })
	w.poll = func(ctx context.Context) ([]byte, error) {
		entered.Store(true)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.start(ctx)
	waitForCondition(t, "the poll to start", entered.Load)
	cancel()

	if !returnsWithin(time.Second, w.wait) {
		t.Fatal("wait() did not return within 1s of the cancel")
	}
	if got := triggers.Load(); got != 0 {
		t.Errorf("triggers = %d, want 0 for a poll cut short by the cancel", got)
	}
}

func TestFRRWatcherNilIsANoOp(t *testing.T) {
	var w *frrWatcher
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.start(ctx)
	if !returnsWithin(time.Second, w.wait) {
		t.Error("wait() on a nil watcher blocked")
	}

	notStarted := newFRRWatcher(func() {})
	if !returnsWithin(time.Second, notStarted.wait) {
		t.Error("wait() blocked on a watcher that was never started")
	}
}
