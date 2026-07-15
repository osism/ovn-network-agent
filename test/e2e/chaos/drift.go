package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The data-plane drift class is the fat-fingered-operator fault: a human
// deletes a rule the agent owns, and the agent must notice and re-install
// it on its next reconcile. These are the exact deletions
// test/integration/scenario_drift_test.go proves the agent heals on a
// single host — driven here against the multi-node lab under fault load.
//
// The deletion is the fault; the restore is a deliberate no-op, because the
// agent's periodic reconcile is the undo. Each action carries a live
// `applicable` so a gateway that does not carry the object is a journaled
// skip, not a no-op deletion: the MAC-tweak flows exist only where routers
// are locally active, and a port-forward-only profile has no FIP routes at
// all.

const (
	// driftFIP is the bootstrap FIP, present and probed in every profile that
	// connects to OVN. Its data-plane state is what these actions delete.
	driftFIP = "192.0.2.10"

	// driftFRRNexthop is the veth_nexthop the agent's FRR static routes point
	// at (gwnode-config.yaml). staticd's `no ip route` form needs it.
	driftFRRNexthop = "169.254.0.1"

	// The OVS flow cookies and the nftables table the agent owns, duplicated
	// here because the chaos runner is package main and cannot import the
	// agent's root package. Kept in step with ovs.go and nftables.go.
	driftHairpinCookie  = "0x998" // ovs.go: ovsCookieHairpin
	driftMACTweakCookie = "0x999" // ovs.go: ovsCookieMACTweak
	driftNftTable       = "ovn-network-agent"
)

// driftActions is the data-plane drift fault class. Every action holds
// nothing (the deletion is instantaneous) and self-heals on the next
// reconcile, so the budget is the worst-case cadence — 15 s after a
// cadence flip — plus probe slack.
func driftActions(l *lab) []*action {
	return []*action{
		{
			name:           "kernel-route-drop",
			weight:         2,
			scope:          scopeGateway,
			object:         driftFIP + "/32 dev br-ex",
			recoveryBudget: 60 * time.Second,
			applicable: func(ctx context.Context, gw string, _ int) bool {
				out, err := l.sh(ctx, gw, "ip route show "+driftFIP+"/32 dev br-ex")
				return err == nil && strings.TrimSpace(out) != ""
			},
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				if _, err := l.exec(ctx, gw, "ip", "route", "del", driftFIP+"/32", "dev", "br-ex"); err != nil {
					return fmt.Errorf("drop the kernel route for %s on %s: %w", driftFIP, gw, err)
				}
				return nil
			},
			restore: driftSelfHeals,
		},
		{
			name:           "frr-route-drop",
			weight:         2,
			scope:          scopeGateway,
			object:         driftFIP + "/32 vrf vrf-provider",
			recoveryBudget: 60 * time.Second,
			applicable: func(ctx context.Context, gw string, _ int) bool {
				_, err := l.sh(ctx, gw, "vtysh -c 'show running-config' | grep -q 'ip route "+
					driftFIP+"/32 "+driftFRRNexthop+"'")
				return err == nil
			},
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				// staticd requires prefix + nexthop on the `no` form; a bare
				// `no ip route <prefix>` is rejected as incomplete.
				if _, err := l.exec(ctx, gw, "vtysh",
					"-c", "conf t",
					"-c", "vrf vrf-provider",
					"-c", "no ip route "+driftFIP+"/32 "+driftFRRNexthop,
					"-c", "exit-vrf",
					"-c", "end"); err != nil {
					return fmt.Errorf("drop the FRR route for %s on %s: %w", driftFIP, gw, err)
				}
				return nil
			},
			restore: driftSelfHeals,
		},
		{
			name:           "nft-flush",
			weight:         2,
			scope:          scopeGateway,
			object:         "table ip " + driftNftTable,
			recoveryBudget: 60 * time.Second,
			applicable: func(ctx context.Context, gw string, _ int) bool {
				_, err := l.exec(ctx, gw, "nft", "list", "table", "ip", driftNftTable)
				return err == nil
			},
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				if _, err := l.exec(ctx, gw, "nft", "flush", "table", "ip", driftNftTable); err != nil {
					return fmt.Errorf("flush the agent's nftables table on %s: %w", gw, err)
				}
				return nil
			},
			restore: driftSelfHeals,
		},
		{
			name:           "ovs-flow-drop",
			weight:         2,
			scope:          scopeGateway,
			object:         "cookies " + driftHairpinCookie + "," + driftMACTweakCookie + " on br-ex",
			recoveryBudget: 60 * time.Second,
			// The MAC-tweak flows exist only where the routers are locally
			// active, so a gateway that carries none is a skip. Removing them
			// darkens external probes and is measured; the hairpin flow's
			// restoration is not probe-observable from client-1 — asserting
			// its presence is the config-aware oracle of issue #179.
			applicable: func(ctx context.Context, gw string, _ int) bool {
				_, err := l.sh(ctx, gw, "ovs-ofctl dump-flows br-ex 'cookie="+
					driftMACTweakCookie+"/-1' | grep -q cookie")
				return err == nil
			},
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				for _, cookie := range []string{driftHairpinCookie, driftMACTweakCookie} {
					if _, err := l.exec(ctx, gw, "ovs-ofctl", "del-flows", "br-ex",
						"cookie="+cookie+"/-1"); err != nil {
						return fmt.Errorf("drop the %s flows on %s: %w", cookie, gw, err)
					}
				}
				return nil
			},
			restore: driftSelfHeals,
		},
	}
}

// driftSelfHeals is the restore for the drift class: the deletion is the
// fault and the agent's next reconcile is the undo — the same self-heal
// contract scenario_drift_test.go proves on a single host — so the runner
// puts nothing back.
func driftSelfHeals(context.Context, *lab, string) error { return nil }
