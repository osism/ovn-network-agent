package main

import (
	"context"
	"strings"
	"testing"
)

func driftActionNamed(t *testing.T, l *lab, name string) *action {
	t.Helper()
	for _, a := range driftActions(l) {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no drift action named %q", name)
	return nil
}

// Each drift action is applicable only when its object is actually present
// on the gateway — otherwise it would be a no-op deletion, and a journaled
// skip is the honest record instead.
func TestDriftAppliesOnlyWhenTheObjectIsPresent(t *testing.T) {
	tests := []struct {
		action  string
		probe   string
		present func([]string) (string, error)
		absent  func([]string) (string, error)
	}{
		{
			action:  "kernel-route-drop",
			probe:   "ip route show 192.0.2.10/32 dev br-ex",
			present: func([]string) (string, error) { return "192.0.2.10 dev br-ex scope link\n", nil },
			absent:  func([]string) (string, error) { return "", nil }, // ip route show exits 0 with no output
		},
		{
			action:  "frr-route-drop",
			probe:   "grep -q 'ip route 192.0.2.10/32 169.254.0.1'",
			present: func([]string) (string, error) { return "", nil },
			absent:  func([]string) (string, error) { return "", errExit(t, 1) },
		},
		{
			action:  "nft-flush",
			probe:   "nft list table ip ovn-network-agent",
			present: func([]string) (string, error) { return "table ip ovn-network-agent {\n}\n", nil },
			absent:  func([]string) (string, error) { return "", errExit(t, 1) },
		},
		{
			action:  "ovs-flow-drop",
			probe:   "cookie=0x999/-1' | grep -q cookie",
			present: func([]string) (string, error) { return "", nil },
			absent:  func([]string) (string, error) { return "", errExit(t, 1) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			for _, present := range []bool{true, false} {
				respond := tc.absent
				if present {
					respond = tc.present
				}
				cmd := &fakeCommander{respond: func(argv []string) (string, error) {
					if strings.Contains(strings.Join(argv, " "), tc.probe) {
						return respond(argv)
					}
					return healthyLabResponses(argv)
				}}
				l := newTestLab(cmd, newFakeClock())
				act := driftActionNamed(t, l, tc.action)

				if got := act.applicable(context.Background(), "gateway-1", 0); got != present {
					t.Fatalf("%s applicable = %v with the object present=%v", tc.action, got, present)
				}
			}
		})
	}
}

// The inject issues exactly the documented deletion, and the restore issues
// nothing at all — the agent's next reconcile is the undo.
func TestDriftInjectsTheDeletionAndRestoresNothing(t *testing.T) {
	tests := []struct {
		action string
		want   []string
	}{
		{"kernel-route-drop", []string{"ip route del 192.0.2.10/32 dev br-ex"}},
		{"frr-route-drop", []string{
			"vtysh -c conf t -c vrf vrf-provider -c no ip route 192.0.2.10/32 169.254.0.1 -c exit-vrf -c end",
		}},
		{"nft-flush", []string{"nft flush table ip ovn-network-agent"}},
		{"ovs-flow-drop", []string{
			"ovs-ofctl del-flows br-ex cookie=0x998/-1",
			"ovs-ofctl del-flows br-ex cookie=0x999/-1",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.action, func(t *testing.T) {
			cmd := &fakeCommander{respond: healthyLabResponses}
			l := newTestLab(cmd, newFakeClock())
			act := driftActionNamed(t, l, tc.action)

			if err := act.inject(context.Background(), l, "gateway-1", 0); err != nil {
				t.Fatalf("inject: %v", err)
			}
			for _, w := range tc.want {
				if !cmd.called(w) {
					t.Fatalf("%s did not issue %q: %v", tc.action, w, cmd.lines())
				}
			}

			restoreCmd := &fakeCommander{respond: healthyLabResponses}
			rl := newTestLab(restoreCmd, newFakeClock())
			if err := act.restore(context.Background(), rl, "gateway-1"); err != nil {
				t.Fatalf("restore: %v", err)
			}
			if len(restoreCmd.lines()) != 0 {
				t.Fatalf("%s restore issued commands, want none (the reconcile self-heals): %v",
					tc.action, restoreCmd.lines())
			}
		})
	}
}

// The flow drop removes the hairpin cookie before the MAC-tweak cookie —
// both are the agent's, and the issue asks for a hairpin or MAC-rewrite
// flow removed.
func TestOVSFlowDropRemovesBothCookies(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := driftActionNamed(t, l, "ovs-flow-drop").
		inject(context.Background(), l, "gateway-1", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	hairpin := cmd.indexOf("del-flows br-ex cookie=0x998/-1")
	macTweak := cmd.indexOf("del-flows br-ex cookie=0x999/-1")
	if hairpin < 0 || macTweak < 0 {
		t.Fatalf("the flow drop did not remove both cookies: %v", cmd.lines())
	}
	if hairpin > macTweak {
		t.Fatalf("the cookies were removed out of order: %v", cmd.lines())
	}
}
