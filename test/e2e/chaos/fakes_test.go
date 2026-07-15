package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// errBoom is the canned failure the tests inject at the commander seam.
// It stands for the failures that are not the command's own verdict — a
// docker daemon under load, an expired cmdTimeout, a container that is
// gone.
var errBoom = errors.New("boom")

// errExit is what the real commander hands back when the command *inside*
// the container exits non-zero: an *exec.ExitError carrying the code,
// wrapped the way execCommander wraps it. pgrep's exit 1 ("nothing
// matched") is an answer; errBoom is a question that could not be asked.
func errExit(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("sh -c 'exit %d' reported success", code)
	}
	return fmt.Errorf("docker exec: %w: ", err)
}

// fakeCommander stands in for the docker / containerlab CLIs. It is the
// only seam the tests replace: the engine, the actions and the state
// layering all run for real against it, so what the tests assert on is
// the argv the runner would have executed.
type fakeCommander struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(argv []string) (string, error)
}

func (f *fakeCommander) run(_ context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return "", nil
	}
	return respond(argv)
}

// lines renders every recorded call as a single string, in order.
func (f *fakeCommander) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, argv := range f.calls {
		out = append(out, strings.Join(argv, " "))
	}
	return out
}

// indexOf returns the position of the first recorded call containing
// substr, or -1. Ordering assertions ("the restart policy is disabled
// before the kill") compare two of these.
func (f *fakeCommander) indexOf(substr string) int {
	for i, line := range f.lines() {
		if strings.Contains(line, substr) {
			return i
		}
	}
	return -1
}

func (f *fakeCommander) called(substr string) bool { return f.indexOf(substr) >= 0 }

// count is how many recorded calls contain substr — "the second call
// issued nothing new" is an assertion on this.
func (f *fakeCommander) count(substr string) int {
	n := 0
	for _, line := range f.lines() {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// healthyLabResponses answers every query the runner makes about a lab
// that is behaving: daemons up, chassis registered, cr-lr0-public bound.
// The one thing it models as gone is eth1 — a container lifecycle event
// destroys the containerlab veth, which is the whole reason the restore
// path exists.
func healthyLabResponses(argv []string) (string, error) {
	line := strings.Join(argv, " ")
	switch {
	case strings.Contains(line, "{{.State.Health.Status}}"):
		return "healthy\n", nil
	case strings.Contains(line, "{{.State.Running}}"):
		return "false\n", nil // a killed container stays down until started
	case strings.Contains(line, "ip link show eth1"):
		return "Device \"eth1\" does not exist.", errBoom
	case strings.Contains(line, "pgrep -f /usr/local/bin/ovn-network-agent"):
		return "1\n", nil
	case strings.Contains(line, "pgrep -x bgpd"):
		return "1\n", nil // a healthy upstream runs bgpd
	case strings.Contains(line, "find Chassis name="):
		return strings.TrimPrefix(argv[len(argv)-1], "name=") + "\n", nil
	case strings.Contains(line, "--columns=chassis find Port_Binding"):
		return "0e3d-master-uuid\n", nil
	case strings.Contains(line, "--columns=name list Chassis"):
		return "gateway-1\n", nil
	}
	return "", nil
}

// fakeClock advances only when the code under test sleeps, so a
// ten-minute run takes microseconds and the tick loop stays
// deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// wait is the engine's ctx-aware wait on fake time: a cancelled run does
// not idle out the rest of its tick interval or its fault hold.
func (c *fakeClock) wait(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.sleep(d)
	return ctx.Err() == nil
}

// greenProbes is a probeSource whose targets are always up.
type greenProbes struct{}

func (greenProbes) allGreen() bool       { return true }
func (greenProbes) redTargets() []string { return nil }
func (greenProbes) recoverySince(time.Time) map[string]int64 {
	return map[string]int64{"fip-vm1": 0}
}

// redProbes is a probeSource whose data path never comes back: the node
// is up, but nothing behind it answers.
type redProbes struct{}

func (redProbes) allGreen() bool       { return false }
func (redProbes) redTargets() []string { return []string{"fip-vm1"} }
func (redProbes) recoverySince(time.Time) map[string]int64 {
	return map[string]int64{"fip-vm1": 0}
}

// testProfile resolves a profile the tests drive the runner with. Most of
// them want the default one — every layer up, every gateway on the baked
// config — which is what the runner did before profiles existed.
func testProfile(t *testing.T, name string) *profile {
	t.Helper()
	p, err := profileByName(name)
	if err != nil {
		t.Fatalf("profileByName(%q): %v", name, err)
	}
	return p
}

func defaultTestProfile(t *testing.T) *profile { return testProfile(t, defaultProfileName) }

// newTestLab wires a lab against the fake commander and the fake clock.
func newTestLab(cmd commander, clock *fakeClock) *lab {
	return &lab{name: "ovn-e2e", cmd: cmd, sleep: clock.sleep, now: clock.now}
}

// noopActions builds a registry whose faults do nothing but record that
// they ran — the engine's decisions are what the engine tests are about.
func noopActions(names ...string) []*action {
	actions := make([]*action, 0, len(names))
	for _, name := range names {
		actions = append(actions, &action{
			name:           name,
			weight:         1,
			holdMin:        5 * time.Second,
			holdMax:        5 * time.Second,
			recoveryBudget: 60 * time.Second,
			inject:         func(context.Context, *lab, string, int) error { return nil },
			restore:        func(context.Context, *lab, string) error { return nil },
		})
	}
	return actions
}

// decisionsIn extracts the drawn decisions from a journal, in order.
// Two runs with identical inputs must produce identical slices — that is
// the reproducibility contract.
func decisionsIn(t *testing.T, journal string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(journal), "\n") {
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("journal line is not valid JSON: %v (%q)", err, line)
		}
		if e.Event != evDecision {
			continue
		}
		out = append(out, strings.Join([]string{
			e.Action, e.Target,
			time.Duration(e.IntervalMS).String(),
			time.Duration(e.HoldMS).String(),
			e.Flip,
		}, "|"))
	}
	return out
}

func eventsIn(t *testing.T, journal string) []event {
	t.Helper()
	var out []event
	for _, line := range strings.Split(strings.TrimSpace(journal), "\n") {
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("journal line is not valid JSON: %v (%q)", err, line)
		}
		out = append(out, e)
	}
	return out
}
