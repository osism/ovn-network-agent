package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// The routing-flap class restarts the FRR/BGP daemons — on a gateway and on
// the upstream — and lets the run assert that the announcements return. The
// "announcements return" check is the engine's existing converge gate: the
// probes from client-1 are only reachable over routes the upstream re-learns
// over BGP, so a green probe set is proof the sessions re-established.

// bgpdReadyProbe polls a restarted bgpd until it has registered with
// watchfrr, mirroring start_upstream_bgpd in bootstrap.sh.
const bgpdReadyProbe = "for _ in $(seq 1 30); do " +
	"if vtysh -c 'show daemons' 2>/dev/null | grep -qw bgpd; then break; fi; sleep 1; done"

// flapActions is the routing-flap fault class. The upstream restart is the
// run's widest legitimate blast radius — every FIP announcement withdraws
// until bgpd is back — so it carries the low weight.
func flapActions() []*action {
	return []*action{
		{
			name:   "frr-restart",
			weight: 2,
			scope:  scopeGateway,
			// No hold: the restart is the fault, and the restore brings FRR
			// back and re-asserts the session.
			recoveryBudget: 120 * time.Second,
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				// The exit statuses are hints, exactly as the gwnode entrypoint
				// treats them; readiness is the restore's own probe.
				if _, err := l.sh(ctx, gw,
					"/usr/lib/frr/frrinit.sh stop || true; rm -rf /var/tmp/frr/*; "+
						"/usr/lib/frr/frrinit.sh start || true"); err != nil {
					return fmt.Errorf("restart FRR on %s: %w", gw, err)
				}
				return nil
			},
			restore: restoreGatewayFRR,
		},
		{
			name:           "upstream-bgp-restart",
			weight:         1,
			scope:          scopeUpstream,
			holdMin:        5 * time.Second,
			holdMax:        20 * time.Second,
			recoveryBudget: 90 * time.Second,
			inject: func(ctx context.Context, l *lab, _ string, _ int) error {
				return killUpstreamBGPD(ctx, l)
			},
			restore: func(ctx context.Context, l *lab, _ string) error {
				return restoreUpstreamBGPD(ctx, l)
			},
		},
	}
}

// restoreGatewayFRR waits for a restarted gateway's vtysh to answer, then
// re-asserts the BGP session — idempotent and self-cleaning, ending with
// `write memory`. The agent re-adds its static routes on the next
// reconcile, and the converge gate's green probes are the assertion that
// the announcements returned.
func restoreGatewayFRR(ctx context.Context, l *lab, gw string) error {
	if err := l.waitReady(ctx, gw, "vtysh -c 'show version'"); err != nil {
		return fmt.Errorf("wait for FRR on %s: %w", gw, err)
	}
	link, ok := linkFor(gw)
	if !ok {
		return fmt.Errorf("no underlay link known for %s", gw)
	}
	return l.configureGatewayBGP(ctx, gw, link)
}

// killUpstreamBGPD stops bgpd on the upstream node. It never restarts the
// container: watchfrr is PID 1 there, so `frrinit.sh restart` or a
// docker restart would take the container and its five containerlab veths
// down (bootstrap.sh:start_upstream_bgpd documents the exit-137 trap). A
// pkill that matches nothing means bgpd was already down — which is the
// fault in place — so its exit 1 is not an error.
func killUpstreamBGPD(ctx context.Context, l *lab) error {
	if _, err := l.exec(ctx, upstreamNode, "pkill", "-x", "bgpd"); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("stop bgpd on %s: %w", upstreamNode, err)
	}
	return nil
}

// restoreUpstreamBGPD starts bgpd in place — never `frrinit.sh restart`,
// never a docker restart — polls for it to register with watchfrr, then
// reloads the write-memory'd config with `vtysh -b`, mirroring
// start_upstream_bgpd in bootstrap.sh.
func restoreUpstreamBGPD(ctx context.Context, l *lab) error {
	script := strings.Join([]string{
		"pgrep -x bgpd >/dev/null 2>&1 || /usr/lib/frr/bgpd -d -A 127.0.0.1 -u frr -g frr",
		bgpdReadyProbe,
		"vtysh -b",
	}, "; ")
	if _, err := l.sh(ctx, upstreamNode, script); err != nil {
		return fmt.Errorf("restart bgpd on %s: %w", upstreamNode, err)
	}
	return nil
}
