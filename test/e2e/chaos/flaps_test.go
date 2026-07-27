package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func flapActionNamed(t *testing.T, name string) *action {
	t.Helper()
	for _, a := range flapActions() {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no flap action named %q", name)
	return nil
}

// lineContaining returns the first recorded call containing substr, or "".
func lineContaining(f *fakeCommander, substr string) string {
	for _, line := range f.lines() {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}

// The gateway FRR restart backgrounds the recycle inside the container —
// clear the completion marker, then stop FRR, clear the stale watchfrr
// state, start it again and touch the marker, in that order — so the
// docker exec returns immediately instead of riding a slow stop+start
// into cmdTimeout's SIGKILL. The restore gates on the marker before it
// waits for vtysh (the old FRR answers vtysh while still stopping) and
// re-asserts the BGP session so the announcements return.
func TestFRRRestartRecyclesFRRAndReassertsBGP(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())
	act := flapActionNamed(t, "frr-restart")

	if err := act.inject(context.Background(), l, "gateway-1", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	line := lineContaining(cmd, "frrinit.sh")
	unmark := strings.Index(line, "rm -f "+frrRestartDoneMarker)
	stop := strings.Index(line, "frrinit.sh stop")
	wipe := strings.Index(line, "rm -rf /var/tmp/frr/*")
	start := strings.Index(line, "frrinit.sh start")
	mark := strings.Index(line, "touch "+frrRestartDoneMarker)
	if unmark < 0 || stop < 0 || wipe < 0 || start < 0 || mark < 0 {
		t.Fatalf("the FRR restart did not clear the marker, stop, wipe, start and mark: %q", line)
	}
	if unmark >= stop || stop >= wipe || wipe >= start || start >= mark {
		t.Fatalf("the FRR restart ran its steps out of order: %q", line)
	}
	if !strings.HasSuffix(strings.TrimSpace(line), "&") {
		t.Fatalf("the FRR recycle is not backgrounded — a slow stop+start would ride the exec into cmdTimeout: %q", line)
	}

	if err := act.restore(context.Background(), l, "gateway-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	marker := cmd.indexOf("test -f " + frrRestartDoneMarker)
	vtysh := cmd.indexOf("vtysh -c 'show version'")
	if marker < 0 {
		t.Fatalf("the restore did not gate on the recycle marker: %v", cmd.lines())
	}
	if vtysh < 0 {
		t.Fatalf("the restore did not wait for FRR to come back: %v", cmd.lines())
	}
	if marker >= vtysh {
		t.Fatalf("the restore probed vtysh before the recycle finished — the old FRR answers too: %v", cmd.lines())
	}
	if !cmd.called("router bgp 65000 vrf vrf-provider") {
		t.Fatalf("the restore did not re-assert the BGP session: %v", cmd.lines())
	}
}

// The completion marker carries the whole backgrounded recycle — stop,
// clear, start — not one daemon coming ready. Zombie daemons under the
// old non-reaping PID 1 took it past the 60s daemon budget (run
// 29516365849) with the lab-state dump showing FRR healthy moments after
// the abort, and even with tini reaping, a loaded runner may stretch the
// recycle. The restore must wait it out on the recycle's own budget
// instead of parking the node over a restart that is merely slow.
func TestFRRRestartRestoreOutwaitsARecycleSlowerThanTheDaemonBudget(t *testing.T) {
	clock := newFakeClock()
	markerAt := clock.now().Add(daemonReadyTimeout + 30*time.Second)
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if strings.Contains(line, "test -f "+frrRestartDoneMarker) && clock.now().Before(markerAt) {
			return "", errExit(t, 1)
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, clock)
	act := flapActionNamed(t, "frr-restart")

	if err := act.restore(context.Background(), l, "gateway-1"); err != nil {
		t.Fatalf("restore gave up on a recycle slower than the daemon budget: %v", err)
	}
}

// The upstream BGP restart stops bgpd in place and its restore starts it in
// place and reloads the config — it must never restart or kill the upstream
// container, whose PID 1 is watchfrr (a docker restart would take the
// container and its containerlab veths down).
func TestUpstreamBGPRestartNeverRecyclesTheContainer(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())
	act := flapActionNamed(t, "upstream-bgp-restart")

	if err := act.inject(context.Background(), l, upstreamNode, 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !cmd.called("exec clab-ovn-e2e-upstream pkill -f " + bgpdMatchPattern) {
		t.Fatalf("the inject did not stop bgpd on the upstream: %v", cmd.lines())
	}

	if err := act.restore(context.Background(), l, upstreamNode); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, want := range []string{
		"bgpd -d -A 127.0.0.1 -u frr -g frr",
		"grep -qw bgpd",
		// The reload must stay guarded: the upstream image has no
		// integrated /etc/frr/frr.conf (write memory persists per-daemon
		// files), and an unguarded `vtysh -b` exits 11 on the missing
		// file — run 30261099950 parked the node over it while bgpd was
		// already back up and configured from /etc/frr/bgpd.conf.
		"[ ! -r /etc/frr/frr.conf ] || vtysh -b",
	} {
		if !cmd.called(want) {
			t.Fatalf("the restore did not start bgpd in place and reload: missing %q in %v", want, cmd.lines())
		}
	}

	for _, line := range cmd.lines() {
		if strings.Contains(line, "clab-ovn-e2e-upstream") &&
			(strings.Contains(line, "docker restart") || strings.Contains(line, "docker kill") ||
				strings.Contains(line, "frrinit.sh restart")) {
			t.Fatalf("the upstream container was recycled — watchfrr is PID 1: %q", line)
		}
	}
}

// A pkill that matches nothing means bgpd was already down, which is the
// fault in place — not a failure. Any other pkill failure must surface.
func TestKillUpstreamBGPDToleratesNoMatchButNotRealFailure(t *testing.T) {
	noMatch := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pkill -f "+bgpdMatchPattern) {
			return "", errExit(t, 1)
		}
		return healthyLabResponses(argv)
	}}
	if err := killUpstreamBGPD(context.Background(), newTestLab(noMatch, newFakeClock())); err != nil {
		t.Fatalf("a no-match pkill (bgpd already down) was treated as an error: %v", err)
	}

	broken := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pkill -f "+bgpdMatchPattern) {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	if err := killUpstreamBGPD(context.Background(), newTestLab(broken, newFakeClock())); err == nil {
		t.Fatal("a real pkill failure was swallowed")
	}
}
