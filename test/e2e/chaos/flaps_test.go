package main

import (
	"context"
	"strings"
	"testing"
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

// The gateway FRR restart stops FRR, clears the stale watchfrr state, and
// starts it again — in that order — then its restore waits for vtysh and
// re-asserts the BGP session so the announcements return.
func TestFRRRestartRecyclesFRRAndReassertsBGP(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())
	act := flapActionNamed(t, "frr-restart")

	if err := act.inject(context.Background(), l, "gateway-1", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	line := lineContaining(cmd, "frrinit.sh")
	stop := strings.Index(line, "frrinit.sh stop")
	wipe := strings.Index(line, "rm -rf /var/tmp/frr/*")
	start := strings.Index(line, "frrinit.sh start")
	if stop < 0 || wipe < 0 || start < 0 {
		t.Fatalf("the FRR restart did not stop, clear and start: %q", line)
	}
	if stop >= wipe || wipe >= start {
		t.Fatalf("the FRR restart ran its steps out of order: %q", line)
	}

	if err := act.restore(context.Background(), l, "gateway-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !cmd.called("vtysh -c 'show version'") {
		t.Fatalf("the restore did not wait for FRR to come back: %v", cmd.lines())
	}
	if !cmd.called("router bgp 65000 vrf vrf-provider") {
		t.Fatalf("the restore did not re-assert the BGP session: %v", cmd.lines())
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
	if !cmd.called("exec clab-ovn-e2e-upstream pkill -x bgpd") {
		t.Fatalf("the inject did not stop bgpd on the upstream: %v", cmd.lines())
	}

	if err := act.restore(context.Background(), l, upstreamNode); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, want := range []string{
		"bgpd -d -A 127.0.0.1 -u frr -g frr",
		"grep -qw bgpd",
		"vtysh -b",
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
		if strings.Contains(strings.Join(argv, " "), "pkill -x bgpd") {
			return "", errExit(t, 1)
		}
		return healthyLabResponses(argv)
	}}
	if err := killUpstreamBGPD(context.Background(), newTestLab(noMatch, newFakeClock())); err != nil {
		t.Fatalf("a no-match pkill (bgpd already down) was treated as an error: %v", err)
	}

	broken := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "pkill -x bgpd") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	if err := killUpstreamBGPD(context.Background(), newTestLab(broken, newFakeClock())); err == nil {
		t.Fatal("a real pkill failure was swallowed")
	}
}
