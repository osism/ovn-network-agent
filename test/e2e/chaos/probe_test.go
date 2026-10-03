package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every target that names no vantage is sourced from client-1, the
// external vantage point the scenarios probe from. The same-node targets
// carry their own node and netns and are covered below.
func TestProbesAreSourcedFromTheClient(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())
	p := newProber(l, defaultProbes, newJournal(&bytes.Buffer{}, newFakeClock().now), newFakeClock().now)

	var external []probeTarget
	for _, target := range defaultProbes {
		if target.node == "" && target.netns == "" {
			external = append(external, target)
			p.sample(context.Background(), target)
		}
	}

	want := []string{
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 192.0.2.10",
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 192.0.2.12",
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 198.51.100.10",
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 203.0.113.10",
		"docker exec clab-ovn-e2e-client-1 curl --silent --max-time 3 --output /dev/null http://192.0.2.50:80/",
	}
	if len(external) != len(want) {
		t.Fatalf("%d default probes name no vantage, want %d", len(external), len(want))
	}
	got := cmd.lines()
	if len(got) != len(want) {
		t.Fatalf("issued %d probes for %d targets: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("probe %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A target that names neither a node nor a netns must still probe from
// client-1, with the argv it had before vantages existed — otherwise
// every recorded run's probe stream would change meaning.
func TestZeroVantageProbesAreUnchanged(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	if err := probeOnce(context.Background(),
		l, probeTarget{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"}); err != nil {
		t.Fatalf("probeOnce() error: %v", err)
	}

	want := "docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 192.0.2.10"
	if got := cmd.lines(); len(got) != 1 || got[0] != want {
		t.Fatalf("zero-vantage probe = %v, want exactly %q", got, want)
	}
}

// A vantage inside the lab enters the target's node and namespace. The
// TCP form is bash's /dev/tcp redirect because the gateway image has no
// curl.
func TestVantageProbesEnterTheirNodeAndNetns(t *testing.T) {
	cases := []struct {
		name   string
		target probeTarget
		want   string
	}{
		{
			name:   "ping",
			target: probeTarget{name: "hairpin-fip", kind: probePing, addr: "192.0.2.12", node: "gateway-3", netns: "vm1"},
			want:   "docker exec clab-ovn-e2e-gateway-3 ip netns exec vm1 ping -c 1 -W 1 192.0.2.12",
		},
		{
			name:   "tcp",
			target: probeTarget{name: "hairpin-vip", kind: probeTCP, addr: "198.18.0.50:8080", node: "gateway-3", netns: "vm1"},
			want:   "docker exec clab-ovn-e2e-gateway-3 ip netns exec vm1 timeout 3 bash -c exec 3<>/dev/tcp/198.18.0.50/8080",
		},
		{
			name:   "tcp without a netns stays in the node's default namespace",
			target: probeTarget{name: "tcp-node", kind: probeTCP, addr: "198.18.0.50:8080", node: "gateway-1"},
			want:   "docker exec clab-ovn-e2e-gateway-1 timeout 3 bash -c exec 3<>/dev/tcp/198.18.0.50/8080",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommander{}
			l := newTestLab(cmd, newFakeClock())

			if err := probeOnce(context.Background(), l, tc.target); err != nil {
				t.Fatalf("probeOnce() error: %v", err)
			}
			if got := cmd.lines(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("probe = %v, want exactly %q", got, tc.want)
			}
		})
	}
}

// A `docker exec` that could not run at all — the node is down, the netns
// went with it mid-fault — is the path being down, so it must record as
// loss rather than stopping the sampler.
func TestVantageProbeExecFailureIsLoss(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	clock := newFakeClock()
	target := probeTarget{name: "hairpin-fip", kind: probePing, addr: "192.0.2.12", node: "gateway-3", netns: "vm1"}
	p := newProber(newTestLab(cmd, clock), []probeTarget{target},
		newJournal(&bytes.Buffer{}, clock.now), clock.now)

	p.sample(context.Background(), target)
	p.sample(context.Background(), target)

	if sum := p.summary()["hairpin-fip"]; sum.Sent != 2 || sum.Lost != 2 {
		t.Fatalf("sent/lost = %d/%d, want 2/2 — a failed exec is loss", sum.Sent, sum.Lost)
	}
	if red := p.redTargets(); len(red) != 1 {
		t.Errorf("redTargets = %v: a target whose every probe failed must read as red", red)
	}
}

// A malformed TCP target is a programming error in the registry, not a
// dead path, so it must not be silently probed as something else.
func TestTCPProbeRejectsATargetWithoutAPort(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	err := probeOnce(context.Background(),
		l, probeTarget{name: "broken", kind: probeTCP, addr: "198.18.0.50", node: "gateway-3"})
	if err == nil || !strings.Contains(err.Error(), "not host:port") {
		t.Fatalf("expected a host:port error, got %v", err)
	}
	if got := cmd.lines(); len(got) != 0 {
		t.Fatalf("a malformed target must exec nothing, got %v", got)
	}
}

// A failing probe is loss, and the red→green edge is journaled so a
// reader can line the outage up against the fault that caused it.
func TestSampleRecordsLossAndJournalsTransitions(t *testing.T) {
	var down bool
	cmd := &fakeCommander{respond: func([]string) (string, error) {
		if down {
			return "", errBoom
		}
		return "", nil
	}}
	clock := newFakeClock()
	var buf bytes.Buffer
	target := probeTarget{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"}
	p := newProber(newTestLab(cmd, clock), []probeTarget{target},
		newJournal(&buf, clock.now), clock.now)

	p.sample(context.Background(), target) // green
	down = true
	p.sample(context.Background(), target) // red
	down = false
	p.sample(context.Background(), target) // green again

	sum := p.summary()["fip-vm1"]
	if sum.Sent != 3 || sum.Lost != 1 {
		t.Fatalf("sent/lost = %d/%d, want 3/1", sum.Sent, sum.Lost)
	}
	if sum.Transitions != 2 {
		t.Fatalf("transitions = %d, want 2", sum.Transitions)
	}
	var down1, up1 bool
	for _, ev := range eventsIn(t, buf.String()) {
		if ev.Event != evProbeTransition || ev.Probe != "fip-vm1" || ev.Up == nil {
			continue
		}
		if *ev.Up {
			up1 = true
		} else {
			down1 = true
		}
	}
	if !down1 || !up1 {
		t.Fatalf("both probe transitions were not journaled: %q", buf.String())
	}
}

// A probe cancelled mid-flight because the run ended is not loss — it
// would otherwise pollute the final loss bucket of every run. Nor does it
// break the green streak a confirmation counts.
func TestSampleIgnoresACancelledProbe(t *testing.T) {
	down := true
	cmd := &fakeCommander{respond: func([]string) (string, error) {
		if down {
			return "", errBoom
		}
		return "", nil
	}}
	clock := newFakeClock()
	target := probeTarget{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"}
	p := newProber(newTestLab(cmd, clock), []probeTarget{target},
		newJournal(&bytes.Buffer{}, clock.now), clock.now)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.sample(ctx, target)

	if sum := p.summary()["fip-vm1"]; sum.Sent != 0 || sum.Lost != 0 {
		t.Fatalf("a cancelled probe was counted: sent/lost = %d/%d", sum.Sent, sum.Lost)
	}

	anchor := clock.now()
	down = false
	p.sample(context.Background(), target)
	p.sample(context.Background(), target)
	before := p.unconfirmedSince(anchor)
	if len(before) != 0 {
		t.Fatalf("two green samples left %v unconfirmed", before)
	}
	down = true
	p.sample(ctx, target)
	if after := p.unconfirmedSince(anchor); !slices.Equal(before, after) {
		t.Fatalf("a cancelled probe changed the unconfirmed probes from %v to %v", before, after)
	}
	if sum := p.summary()["fip-vm1"]; sum.Sent != 2 || sum.Lost != 0 {
		t.Fatalf("a cancelled probe was counted: sent/lost = %d/%d, want 2/0", sum.Sent, sum.Lost)
	}
}

// confirmFixture is a prober over fip-vm1 whose every probe takes 80 ms of
// fake time and fails while down is set. between, when set, runs half-way
// through a probe, 40 ms after it started.
type confirmFixture struct {
	clock   *fakeClock
	p       *prober
	down    bool
	between func()
}

var confirmTarget = probeTarget{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"}

func newConfirmFixture() *confirmFixture {
	f := &confirmFixture{clock: newFakeClock()}
	cmd := &fakeCommander{respond: func([]string) (string, error) {
		f.clock.sleep(40 * time.Millisecond)
		if f.between != nil {
			f.between()
		}
		f.clock.sleep(40 * time.Millisecond)
		if f.down {
			return "", errBoom
		}
		return "", nil
	}}
	f.p = newProber(newTestLab(cmd, f.clock), []probeTarget{confirmTarget},
		newJournal(&bytes.Buffer{}, f.clock.now), f.clock.now)
	return f
}

// sample runs one green or red probe of fip-vm1.
func (f *confirmFixture) sample(t *testing.T, up bool) {
	t.Helper()
	f.down = !up
	f.p.sample(t.Context(), confirmTarget)
}

// wantListed asserts whether unconfirmedSince(anchor) names fip-vm1.
func (f *confirmFixture) wantListed(t *testing.T, anchor time.Time, listed bool, why string) {
	t.Helper()
	var want []string
	if listed {
		want = []string{"fip-vm1"}
	}
	if got := f.p.unconfirmedSince(anchor); !slices.Equal(got, want) {
		t.Fatalf("%s: unconfirmedSince(t+%s) = %v, want %v",
			why, anchor.Sub(f.p.startedAt), got, want)
	}
}

// A fault is converged only once every probe could have seen it: a target
// counts as confirmed green after two consecutive green samples that both
// started at or after the anchor, the restore.
func TestUnconfirmedSinceNeedsTwoGreenSamplesStartedAfterTheAnchor(t *testing.T) {
	t.Run("one green sample is not enough, the second confirms", func(t *testing.T) {
		f := newConfirmFixture()
		anchor := f.clock.now()
		f.sample(t, true)
		f.wantListed(t, anchor, true, "after one green sample")
		f.sample(t, true)
		f.wantListed(t, anchor, false, "after two green samples")
	})

	t.Run("a sample in flight at the anchor does not count", func(t *testing.T) {
		f := newConfirmFixture()
		var anchor time.Time
		f.between = func() {
			anchor = f.clock.now()
			f.between = nil
		}
		f.sample(t, true) // started 40 ms before the anchor, returned after it
		f.sample(t, true)
		f.wantListed(t, anchor, true, "after the in-flight sample and one more")
		f.sample(t, true)
		f.wantListed(t, anchor, false, "after two green samples started after the anchor")
	})

	t.Run("a red sample empties the streak", func(t *testing.T) {
		f := newConfirmFixture()
		anchor := f.clock.now()
		f.sample(t, true)
		f.sample(t, true)
		f.sample(t, false)
		f.sample(t, true)
		f.wantListed(t, anchor, true, "after green, green, red, green")
		f.sample(t, true)
		f.wantListed(t, anchor, false, "after two green samples since the red one")
	})

	t.Run("a prober with no targets returns nil", func(t *testing.T) {
		clock := newFakeClock()
		p := newProber(nil, nil, newJournal(&bytes.Buffer{}, clock.now), clock.now)
		if got := p.unconfirmedSince(clock.now()); got != nil {
			t.Fatalf("unconfirmedSince on no targets = %#v, want nil", got)
		}
	})

	t.Run("a target that has not completed a sample is listed", func(t *testing.T) {
		f := newConfirmFixture()
		if red := f.p.redTargets(); len(red) != 0 {
			t.Fatalf("a new target reads as red: %v", red)
		}
		f.wantListed(t, f.clock.now(), true, "before any sample")
	})

	t.Run("the zero anchor counts every sample", func(t *testing.T) {
		f := newConfirmFixture()
		f.sample(t, true)
		f.sample(t, true)
		f.wantListed(t, time.Time{}, false, "after two green samples")
	})

	t.Run("a later anchor un-confirms a target", func(t *testing.T) {
		f := newConfirmFixture()
		anchor := f.clock.now()
		f.sample(t, true)
		f.sample(t, true)
		f.wantListed(t, anchor, false, "after two green samples")
		f.wantListed(t, f.clock.now(), true, "for an anchor after both samples started")
	})

	t.Run("a failed probe command is a red sample", func(t *testing.T) {
		f := newConfirmFixture()
		anchor := f.clock.now()
		f.sample(t, true)
		f.sample(t, true)
		f.sample(t, false) // the commander answers errBoom
		if red := f.p.redTargets(); !slices.Equal(red, []string{"fip-vm1"}) {
			t.Fatalf("redTargets = %v, want [fip-vm1]", red)
		}
		f.wantListed(t, anchor, true, "after a failed probe command")
	})

	t.Run("the names come in target order", func(t *testing.T) {
		clock := newFakeClock()
		targets := []probeTarget{
			{name: "pf-vip", kind: probeHTTP, addr: vipURL},
			{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"},
		}
		p := newProber(nil, targets, newJournal(&bytes.Buffer{}, clock.now), clock.now)
		if got, want := p.unconfirmedSince(clock.now()), []string{"pf-vip", "fip-vm1"}; !slices.Equal(got, want) {
			t.Fatalf("unconfirmedSince = %v, want %v", got, want)
		}
	})

	t.Run("a confirmed target does not hide an unconfirmed one", func(t *testing.T) {
		clock := newFakeClock()
		targets := []probeTarget{
			{name: "pf-vip", kind: probeHTTP, addr: vipURL},
			{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"},
		}
		p := newProber(nil, targets, newJournal(&bytes.Buffer{}, clock.now), clock.now)
		anchor := clock.now()
		p.record("pf-vip", true, clock.now())
		p.record("pf-vip", true, clock.now())
		p.record("fip-vm1", true, clock.now())
		if got, want := p.unconfirmedSince(anchor), []string{"fip-vm1"}; !slices.Equal(got, want) {
			t.Fatalf("unconfirmedSince = %v, want %v", got, want)
		}
	})
}

// The sampling goroutines must stop when the run does, or the runner
// would never exit.
func TestProberStopsWithItsContext(t *testing.T) {
	cmd := &fakeCommander{}
	clock := newFakeClock()
	p := newProber(newTestLab(cmd, clock), defaultProbes,
		newJournal(&bytes.Buffer{}, clock.now), clock.now)

	ctx, cancel := context.WithCancel(context.Background())
	p.start(ctx)
	cancel()

	done := make(chan struct{})
	go func() {
		p.stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the prober did not stop when its context was cancelled")
	}

	// Every target was sampled at least once before the shutdown.
	for _, target := range defaultProbes {
		if !cmd.called(target.addr) && !cmd.called(strings.TrimPrefix(target.addr, "http://")) {
			t.Fatalf("target %s was never probed: %v", target.name, cmd.lines())
		}
	}
}

// A failover followed by a blink at restore is two short red windows, not
// one outage spanning from the inject to the last recovery: downtimeSince
// sums the windows after the anchor while recoverySince keeps dating the
// last red-to-green edge.
func TestDowntimeSinceSumsTheWindowsAfterTheAnchor(t *testing.T) {
	clock := newFakeClock()
	targets := []probeTarget{
		{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"},
		{name: "fip-vm2", kind: probePing, addr: "192.0.2.12"},
	}
	p := newProber(nil, targets, newJournal(&bytes.Buffer{}, clock.now), clock.now)
	downtime := func(anchor time.Time, probe string, wantMS int64, wantWindows int) {
		t.Helper()
		ms, windows := p.downtimeSince(anchor)
		if ms[probe] != wantMS || windows[probe] != wantWindows {
			t.Fatalf("downtimeSince(t+%s)[%s] = %d ms in %d windows, want %d ms in %d",
				anchor.Sub(p.startedAt), probe, ms[probe], windows[probe], wantMS, wantWindows)
		}
	}

	anchor := clock.now()
	p.record("fip-vm1", true, clock.now()) // t+0: already green, no edge
	clock.sleep(2 * time.Second)
	p.record("fip-vm1", false, clock.now()) // t+2: the failover starts
	clock.sleep(500 * time.Millisecond)
	p.record("fip-vm1", false, clock.now()) // t+2.5: still red, no new window
	clock.sleep(700 * time.Millisecond)
	p.record("fip-vm1", true, clock.now()) // t+3.2: failed over
	clock.sleep(36_800 * time.Millisecond)
	p.record("fip-vm1", false, clock.now()) // t+40: the blink at restore
	clock.sleep(90 * time.Millisecond)
	p.record("fip-vm1", true, clock.now()) // t+40.09

	downtime(anchor, "fip-vm1", 1290, 2)
	if got := p.summary()["fip-vm1"].Transitions; got != 4 {
		t.Fatalf("transitions = %d, want 4: a repeated sample is not an edge", got)
	}
	if got := p.recoverySince(anchor)["fip-vm1"]; got != 40_090 {
		t.Fatalf("recoverySince = %d ms, want 40090", got)
	}
	// An anchor inside a window counts only the part after it, and the
	// window once.
	downtime(anchor.Add(2500*time.Millisecond), "fip-vm1", 790, 2)
	downtime(anchor.Add(10*time.Second), "fip-vm1", 90, 1)

	// A window still open counts up to the prober's now.
	clock.sleep(9910 * time.Millisecond)
	p.record("fip-vm1", false, clock.now()) // t+50
	clock.sleep(2 * time.Second)
	downtime(anchor.Add(51*time.Second), "fip-vm1", 1000, 1)

	// A target that never went red is reported, with nothing lost.
	ms, windows := p.downtimeSince(anchor)
	if got, ok := ms["fip-vm2"]; !ok || got != 0 {
		t.Fatalf("fip-vm2 down_ms = %d (present %t), want a 0 entry", got, ok)
	}
	if got, ok := windows["fip-vm2"]; !ok || got != 0 {
		t.Fatalf("fip-vm2 down_windows = %d (present %t), want a 0 entry", got, ok)
	}

	// A window that ended exactly at the anchor is not after it.
	p.record("fip-vm1", true, clock.now()) // t+52
	downtime(clock.now(), "fip-vm1", 0, 0)

	// A target without state is skipped, as recoverySince skips it.
	delete(p.state, "fip-vm2")
	ms, windows = p.downtimeSince(anchor)
	if len(ms) != 1 || len(windows) != 1 {
		t.Fatalf("downtimeSince without fip-vm2's state = %v, %v, want only fip-vm1", ms, windows)
	}
	if _, ok := ms["fip-vm1"]; !ok {
		t.Fatalf("downtimeSince dropped fip-vm1: %v", ms)
	}

	// No targets at all: two empty maps a JSON reader sees as {}.
	empty := newProber(nil, nil, newJournal(&bytes.Buffer{}, clock.now), clock.now)
	ms, windows = empty.downtimeSince(anchor)
	if ms == nil || windows == nil || len(ms) != 0 || len(windows) != 0 {
		t.Fatalf("downtimeSince on no targets = %#v, %#v, want two empty, non-nil maps", ms, windows)
	}
}
