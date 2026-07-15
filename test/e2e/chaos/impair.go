package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The network-impairment class degrades the management path between a
// gateway and the OVN central node without killing anything: it forces the
// gateway's OVSDB connections to central to flap while the container stays
// healthy, which is the quiet failure a lossy or slow management link
// produces. The impairment is scoped to the gateway→central path with a
// u32 filter, so the geneve tunnels between gateways — whose encap IP is
// also eth0 — stay clean and the data plane is not collaterally darkened.

// The fixed netem specs the two impairment actions apply. They are
// constants, not extra draws, so the impairment does not change the number
// of values a tick takes from the stream.
const (
	mgmtLossSpec  = "loss 30%"
	mgmtDelaySpec = "delay 200ms 50ms"
)

// impairActions is the network-impairment fault class. Neither kills
// anything: the container stays healthy and only the connections to central
// flap, which is the point.
func impairActions() []*action {
	return []*action{
		{
			name:           "mgmt-loss",
			weight:         2,
			scope:          scopeGateway,
			object:         "loss 30% → central",
			holdMin:        20 * time.Second,
			holdMax:        60 * time.Second,
			recoveryBudget: 90 * time.Second,
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				return injectMgmtImpair(ctx, l, gw, mgmtLossSpec)
			},
			restore: restoreMgmtImpair,
		},
		{
			name:           "mgmt-delay",
			weight:         2,
			scope:          scopeGateway,
			object:         "delay 200ms → central",
			holdMin:        20 * time.Second,
			holdMax:        60 * time.Second,
			recoveryBudget: 90 * time.Second,
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				return injectMgmtImpair(ctx, l, gw, mgmtDelaySpec)
			},
			restore: restoreMgmtImpair,
		},
	}
}

// injectMgmtImpair applies a netem impairment to only the traffic from gw's
// eth0 to the central node. It builds a prio qdisc whose default band is
// unimpaired, hangs the netem discipline off a spare band, and steers just
// the packets destined for central into it with a u32 filter — so the
// gateway-to-gateway geneve tunnels on the same interface are untouched.
//
// sch_netem cannot be modprobe'd from inside the container (it holds only
// CAP_NET_ADMIN), so it must be loaded on the host — CI does it in
// e2e-lab-setup. The in-container modprobe is a best-effort no-op where the
// module is already loaded; if it is genuinely absent the tc netem command
// below fails and the engine parks the node, which is visible, not silent.
func injectMgmtImpair(ctx context.Context, l *lab, gw, netemSpec string) error {
	central, err := l.mgmtIP(ctx, centralNode)
	if err != nil {
		return fmt.Errorf("resolve the central management address: %w", err)
	}
	script := strings.Join([]string{
		"modprobe sch_netem 2>/dev/null || true",
		"tc qdisc replace dev eth0 root handle 1: prio bands 4 priomap 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0",
		"tc qdisc replace dev eth0 parent 1:4 handle 40: netem " + netemSpec,
		"tc filter add dev eth0 parent 1: protocol ip prio 1 u32 match ip dst " + central + "/32 flowid 1:4",
	}, "; ")
	if _, err := l.sh(ctx, gw, script); err != nil {
		return fmt.Errorf("impair the management path on %s: %w", gw, err)
	}
	return nil
}

// restoreMgmtImpair removes the whole prio+netem+filter hierarchy in one
// step by deleting the root qdisc, reverting eth0 to its default.
func restoreMgmtImpair(ctx context.Context, l *lab, gw string) error {
	if _, err := l.sh(ctx, gw, "tc qdisc del dev eth0 root"); err != nil {
		return fmt.Errorf("remove the management-path impairment on %s: %w", gw, err)
	}
	return nil
}
