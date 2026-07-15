package main

import (
	"context"
	"fmt"
	"time"
)

// The control-plane class targets the shared central node and, for the
// double failover, a pair of gateways. Container kills and agent restarts
// are the visible faults; production incidents are more often the quiet
// ones — a stalled database connection, a second gateway lost while the
// first is still draining. These reuse the mechanics the lab already
// proves: SIGSTOP/SIGCONT against the ovn-ctl pidfiles the central
// entrypoint writes, and the SIGTERM/SIGKILL the starter faults use.

// The ovn-ctl pidfiles inside the central container. central-entrypoint.sh
// runs `ovn-ctl start_northd`, which writes these under /var/run/ovn (the
// standard ovn-ctl run dir the entrypoint creates).
const (
	nbDBPidfile   = "/var/run/ovn/ovnnb_db.pid"
	sbDBPidfile   = "/var/run/ovn/ovnsb_db.pid"
	northdPidfile = "/var/run/ovn/ovn-northd.pid"
)

// controlPlaneActions is the control-plane fault class. The database
// pauses span a short stall — below the OVSDB inactivity probe — and an
// outage long enough to force every ovn-controller and agent to reconnect
// and resync, so one action covers both. The budgets differ by blast
// radius: a paused NB stalls writes, a paused SB stalls every chassis, and
// a double failover is two container bring-ups' worth of re-convergence.
func controlPlaneActions(p *profile) []*action {
	startAndRestore := func(ctx context.Context, l *lab, gw string) error {
		return startAndRestoreGateway(ctx, l, p, gw)
	}
	return []*action{
		{
			name:           "nb-pause",
			weight:         2,
			scope:          scopeCentral,
			object:         "ovnnb_db",
			holdMin:        5 * time.Second,
			holdMax:        90 * time.Second,
			recoveryBudget: 90 * time.Second,
			inject: func(ctx context.Context, l *lab, _ string, _ int) error {
				return pauseCentralProcess(ctx, l, nbDBPidfile)
			},
			restore: func(ctx context.Context, l *lab, _ string) error {
				return resumeCentralProcess(ctx, l, nbDBPidfile)
			},
		},
		{
			name:   "sb-pause",
			weight: 2,
			scope:  scopeCentral,
			object: "ovnsb_db",
			// A paused SB forces every ovn-controller and agent to reconnect
			// and resync. While it is paused the baseline sweep's
			// sbctl --timeout=5 calls fail and are journaled as check-errors,
			// not violations.
			holdMin:        5 * time.Second,
			holdMax:        90 * time.Second,
			recoveryBudget: 120 * time.Second,
			inject: func(ctx context.Context, l *lab, _ string, _ int) error {
				return pauseCentralProcess(ctx, l, sbDBPidfile)
			},
			restore: func(ctx context.Context, l *lab, _ string) error {
				return resumeCentralProcess(ctx, l, sbDBPidfile)
			},
		},
		{
			name:           "northd-pause",
			weight:         1,
			scope:          scopeCentral,
			object:         "ovn-northd",
			holdMin:        10 * time.Second,
			holdMax:        60 * time.Second,
			recoveryBudget: 60 * time.Second,
			inject: func(ctx context.Context, l *lab, _ string, _ int) error {
				return pauseCentralProcess(ctx, l, northdPidfile)
			},
			restore: func(ctx context.Context, l *lab, _ string) error {
				return resumeCentralProcess(ctx, l, northdPidfile)
			},
		},
		{
			name:           "double-failover",
			weight:         1,
			scope:          scopeGatewayPair,
			holdMin:        10 * time.Second,
			holdMax:        30 * time.Second,
			recoveryBudget: 240 * time.Second,
			inject:         injectDoubleFailover,
			// The engine restores each node of the pair in turn, so the same
			// container-lifecycle restore the starter kills use puts both the
			// drained target and the killed peer back.
			restore: startAndRestore,
		},
	}
}

// pauseCentralProcess SIGSTOPs one of central's ovn-ctl-managed processes,
// stalling its OVSDB or northd without tearing the container down.
func pauseCentralProcess(ctx context.Context, l *lab, pidfile string) error {
	if _, err := l.sh(ctx, centralNode, "kill -STOP $(cat "+pidfile+")"); err != nil {
		return fmt.Errorf("pause %s on %s: %w", pidfile, centralNode, err)
	}
	return nil
}

// resumeCentralProcess SIGCONTs a process pauseCentralProcess stopped.
func resumeCentralProcess(ctx context.Context, l *lab, pidfile string) error {
	if _, err := l.sh(ctx, centralNode, "kill -CONT $(cat "+pidfile+")"); err != nil {
		return fmt.Errorf("resume %s on %s: %w", pidfile, centralNode, err)
	}
	return nil
}

// injectDoubleFailover kills a gateway while another is still draining: it
// SIGTERMs the drawn target (which begins its graceful drain, if the
// profile enables it), then SIGKILLs the ring-next peer before the target
// has finished. The peer resolves identically to the engine's own
// nextGateway, so both nodes the engine tracks are the ones that go down.
// Both containers pin their restart policy off first, exactly as the
// gateway-kill and agent-terminate faults do.
func injectDoubleFailover(ctx context.Context, l *lab, target string, _ int) error {
	peer := nextGateway(target)
	if err := l.setRestartPolicy(ctx, target, "no"); err != nil {
		return err
	}
	if err := l.terminateAgent(ctx, target); err != nil {
		return err
	}
	if err := l.setRestartPolicy(ctx, peer, "no"); err != nil {
		return err
	}
	if err := l.killGateway(ctx, peer); err != nil {
		return err
	}
	return l.waitContainerExit(ctx, target, agentExitTimeout)
}
