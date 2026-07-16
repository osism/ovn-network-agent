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

// frrRestartDoneMarker is touched inside a gateway container as the last
// step of the backgrounded FRR recycle. It is the only signal that tells
// the restore the new FRR is the one answering vtysh rather than the old
// instance still shutting down: gated on vtysh alone, the restore would
// re-assert the session against the doomed instance and the converge
// gate would race the restart.
const frrRestartDoneMarker = "/tmp/chaos-frr-restart.done"

// frrRecycleTimeout bounds the wait for the recycle's completion marker.
// The marker carries the whole stop+clear+start, not one daemon coming
// ready. With tini reaping the gateway's orphans (Dockerfile.gwnode) the
// recycle completes in seconds, so three minutes is a pure backstop for a
// genuinely loaded runner. Before tini, every stopped daemon stayed a
// zombie under the non-reaping agent PID 1, `kill -0` kept succeeding on
// it, and frrinit.sh's daemon_stop waited out its full 120 s loop per
// stop phase — a deterministic ~6-minute stop that outlived the 30 s
// cmdTimeout (run 29501890572), the 60 s daemonReadyTimeout (run
// 29516365849) and this budget (run 29525707974) in turn, each abort
// misread as a slow runner because the lab-state dump — taken minutes
// later — showed FRR healthy again.
const frrRecycleTimeout = 3 * time.Minute

// flapActions is the routing-flap fault class. The upstream restart is the
// run's widest legitimate blast radius — every FIP announcement withdraws
// until bgpd is back — so it carries the low weight.
func flapActions() []*action {
	return []*action{
		{
			name:   "frr-restart",
			weight: 2,
			scope:  scopeGateway,
			// No hold: the restart is the fault, and the restore waits out
			// the recycle and re-asserts the session.
			recoveryBudget: 120 * time.Second,
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				// The recycle runs backgrounded inside the container. Run
				// synchronously, a slow stop+start rides the exec into
				// cmdTimeout — and the SIGKILL that follows reaps only
				// the docker exec client, so FRR restarted anyway while the
				// engine recorded an action-failed violation for a lab that
				// was healthy seconds later. The subshell survives the exec
				// returning (stdio detached, reparented to the container's
				// init), and the marker it touches last is the completion
				// signal restoreGatewayFRR gates on. The frrinit exit
				// statuses stay hints, exactly as the gwnode entrypoint
				// treats them.
				if _, err := l.sh(ctx, gw,
					"rm -f "+frrRestartDoneMarker+"; "+
						"(/usr/lib/frr/frrinit.sh stop || true; rm -rf /var/tmp/frr/*; "+
						"/usr/lib/frr/frrinit.sh start || true; "+
						"touch "+frrRestartDoneMarker+") </dev/null >/dev/null 2>&1 &"); err != nil {
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

// restoreGatewayFRR waits out the backgrounded recycle via its completion
// marker — while the recycle is still in its stop phase, the old FRR
// answers vtysh happily — then waits for the restarted vtysh and
// re-asserts the BGP session — idempotent and self-cleaning, ending with
// `write memory`. The marker wait runs on frrRecycleTimeout, not the
// daemon budget: it spans the full stop+clear+start. The agent re-adds
// its static routes on the next reconcile, and the converge gate's green
// probes are the assertion that the announcements returned.
func restoreGatewayFRR(ctx context.Context, l *lab, gw string) error {
	if err := l.waitReadyFor(ctx, gw, "test -f "+frrRestartDoneMarker, frrRecycleTimeout); err != nil {
		return fmt.Errorf("wait for the FRR recycle on %s: %w", gw, err)
	}
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
