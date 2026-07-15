package main

import (
	"context"
	"strings"
	"testing"
)

func impairActionNamed(t *testing.T, name string) *action {
	t.Helper()
	for _, a := range impairActions() {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no impairment action named %q", name)
	return nil
}

// The impairment resolves central's management address and steers only the
// traffic bound for it into the netem band, on the target gateway's eth0 —
// so the gateway-to-gateway geneve tunnels on the same interface stay
// clean. The two actions differ only in the netem discipline.
func TestMgmtImpairScopesNetemToTheCentralPath(t *testing.T) {
	tests := []struct {
		name string
		spec string
	}{
		{"mgmt-loss", "netem loss 30%"},
		{"mgmt-delay", "netem delay 200ms 50ms"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommander{respond: func(argv []string) (string, error) {
				line := strings.Join(argv, " ")
				if strings.Contains(line, "ip -o -4 addr show eth0") {
					return "172.20.20.2\n", nil
				}
				return healthyLabResponses(argv)
			}}
			l := newTestLab(cmd, newFakeClock())
			act := impairActionNamed(t, tc.name)

			if err := act.inject(context.Background(), l, "gateway-1", 0); err != nil {
				t.Fatalf("inject: %v", err)
			}

			for _, want := range []string{
				"clab-ovn-e2e-gateway-1 sh -euc",
				"tc qdisc replace dev eth0 root handle 1: prio",
				tc.spec,
				"u32 match ip dst 172.20.20.2/32 flowid 1:4",
			} {
				if !cmd.called(want) {
					t.Fatalf("%s did not issue %q: %v", tc.name, want, cmd.lines())
				}
			}
			// Central is only read for its address, never impaired: the netem
			// hierarchy lives entirely on the gateway's own egress qdisc.
			for _, line := range cmd.lines() {
				if strings.Contains(line, "clab-ovn-e2e-central") && strings.Contains(line, "tc ") {
					t.Fatalf("%s applied tc inside central: %q", tc.name, line)
				}
			}
		})
	}
}

// The restore deletes the root qdisc, which takes the whole prio+netem+
// filter hierarchy down in one step.
func TestMgmtImpairRestoreRemovesTheRootQdisc(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := impairActionNamed(t, "mgmt-loss").restore(context.Background(), l, "gateway-2"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !cmd.called("clab-ovn-e2e-gateway-2 sh -euc tc qdisc del dev eth0 root") {
		t.Fatalf("the restore did not remove the root qdisc: %v", cmd.lines())
	}
}

// A gateway whose management address cannot be resolved must fail the
// impairment rather than steer traffic at an empty destination.
func TestMgmtImpairFailsWhenCentralAddressIsUnreadable(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "ip -o -4 addr show eth0") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := impairActionNamed(t, "mgmt-delay").inject(context.Background(), l, "gateway-1", 0)
	if err == nil {
		t.Fatal("the impairment was applied without a central address to steer at")
	}
	if !strings.Contains(err.Error(), "central management address") {
		t.Fatalf("error %q does not name the address it could not resolve", err)
	}
}
