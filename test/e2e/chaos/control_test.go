package main

import (
	"context"
	"strings"
	"testing"
)

func controlActionNamed(t *testing.T, name string) *action {
	t.Helper()
	for _, a := range controlPlaneActions(defaultTestProfile(t)) {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no control-plane action named %q", name)
	return nil
}

// The database and northd pauses SIGSTOP/SIGCONT the ovn-ctl process by its
// own pidfile, inside the central container, and touch no other node.
func TestCentralPausesStopAndContTheRightPidfile(t *testing.T) {
	tests := []struct {
		name    string
		pidfile string
	}{
		{"nb-pause", nbDBPidfile},
		{"sb-pause", sbDBPidfile},
		{"northd-pause", northdPidfile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommander{respond: healthyLabResponses}
			l := newTestLab(cmd, newFakeClock())
			act := controlActionNamed(t, tc.name)

			if err := act.inject(context.Background(), l, centralNode, 0); err != nil {
				t.Fatalf("inject: %v", err)
			}
			if err := act.restore(context.Background(), l, centralNode); err != nil {
				t.Fatalf("restore: %v", err)
			}

			want := []string{
				"clab-ovn-e2e-central sh -euc kill -STOP $(cat " + tc.pidfile + ")",
				"clab-ovn-e2e-central sh -euc kill -CONT $(cat " + tc.pidfile + ")",
			}
			for _, w := range want {
				if !cmd.called(w) {
					t.Fatalf("%s did not issue %q: %v", tc.name, w, cmd.lines())
				}
			}
			for _, line := range cmd.lines() {
				if strings.Contains(line, "gateway") || strings.Contains(line, "upstream") {
					t.Fatalf("%s touched a node other than central: %q", tc.name, line)
				}
			}
		})
	}
}

// A paused database is only paused, never killed: nothing signals a
// container or flips a restart policy.
func TestCentralPauseNeverKillsAContainer(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())
	act := controlActionNamed(t, "sb-pause")

	if err := act.inject(context.Background(), l, centralNode, 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	for _, forbidden := range []string{"docker kill", "docker restart", "--restart=no", "docker start"} {
		if cmd.called(forbidden) {
			t.Fatalf("a database pause issued %q: %v", forbidden, cmd.lines())
		}
	}
}

// The double failover kills the ring-next peer while the drawn target is
// still draining: it disables and SIGTERMs the target first, then disables
// and SIGKILLs the peer, and waits only for the target's own exit.
func TestDoubleFailoverKillsThePeerWhileTheTargetDrains(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := controlActionNamed(t, "double-failover").
		inject(context.Background(), l, "gateway-1", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	// The peer is the ring-next gateway, resolved exactly as the engine does.
	if peer := nextGateway("gateway-1"); peer != "gateway-2" {
		t.Fatalf("nextGateway(gateway-1) = %q, want gateway-2", peer)
	}

	disableTarget := cmd.indexOf("update --restart=no clab-ovn-e2e-gateway-1")
	termTarget := cmd.indexOf("clab-ovn-e2e-gateway-1 kill -TERM 1")
	disablePeer := cmd.indexOf("update --restart=no clab-ovn-e2e-gateway-2")
	killPeer := cmd.indexOf("kill -s KILL clab-ovn-e2e-gateway-2")
	if disableTarget < 0 || termTarget < 0 || disablePeer < 0 || killPeer < 0 {
		t.Fatalf("the double failover did not drain the target and kill the peer: %v", cmd.lines())
	}
	if disableTarget >= termTarget || termTarget >= disablePeer || disablePeer >= killPeer {
		t.Fatalf("the peer was not killed while the target was still draining: %v", cmd.lines())
	}

	// Only the target's exit is waited for — the peer is SIGKILLed and gone.
	if !cmd.called("inspect -f {{.State.Running}} clab-ovn-e2e-gateway-1") {
		t.Fatalf("the runner did not wait for the drained target to exit: %v", cmd.lines())
	}
	if cmd.called("inspect -f {{.State.Running}} clab-ovn-e2e-gateway-2") {
		t.Fatalf("the runner waited on the SIGKILLed peer's exit: %v", cmd.lines())
	}
}
