package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// The probe primitives are what every reachability verdict in the run
// rests on: both are sourced from client-1, the external vantage point
// the scenarios probe from.
func TestProbesAreSourcedFromTheClient(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())
	p := newProber(l, defaultProbes, newJournal(&bytes.Buffer{}, newFakeClock().now), newFakeClock().now)

	for _, target := range defaultProbes {
		p.sample(context.Background(), target)
	}

	want := []string{
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 192.0.2.10",
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 192.0.2.12",
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 198.51.100.10",
		"docker exec clab-ovn-e2e-client-1 ping -c 1 -W 1 203.0.113.10",
		"docker exec clab-ovn-e2e-client-1 curl --silent --max-time 3 --output /dev/null http://192.0.2.50:80/",
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
	if p.allGreen() {
		t.Error("a target whose every probe failed must not read as green")
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
// would otherwise pollute the final loss bucket of every run.
func TestSampleIgnoresACancelledProbe(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
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
