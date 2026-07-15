package main

import (
	"context"
	"time"
)

// agentExitTimeout is how long a SIGTERM'ed agent gets to take its
// container down before agent-terminate gives up on the graceful path.
//
// It is generous because a profile (or a drain-toggle flip) can put a
// gateway on drain_on_shutdown, and a draining agent holds its SIGTERM
// for up to drain_timeout — 60s by default — plus the settle delay and
// the cleanup before PID 1 finally exits. The wait returns as soon as the
// container is down, so the larger budget costs a no-drain run nothing.
const agentExitTimeout = 120 * time.Second

// starterActions is the action registry, in the order the weighted pick
// walks it. Both the order and the weights are part of the replay
// contract: the same seed and the same weights replay the same sequence,
// so a new action is appended, never inserted.
//
// Each fault reuses the mechanics an existing scenario already proves:
//
//	controller-restart — failover.sh's clean chassis-loss simulation
//	gateway-kill       — stale-chassis.sh's SIGKILL
//	agent-terminate    — drain-hitless.sh's SIGTERM to PID 1
//	gateway-restart    — pf-hairpin.sh's `docker restart`
//
// The recovery budgets differ because the faults differ: stopping
// ovn-controller costs a re-election, while any container lifecycle
// event costs a full daemon bring-up plus the underlay re-wire.
//
// The profile is bound into the restores because what a returning gateway
// needs put back depends on which layers the profile put up — and, for a
// gateway whose configuration carries the API VIP, on the responder
// behind it.
func starterActions(p *profile) []*action {
	restore := func(ctx context.Context, l *lab, gw string) error {
		return restoreNode(ctx, l, p, gw)
	}
	startAndRestore := func(ctx context.Context, l *lab, gw string) error {
		return startAndRestoreGateway(ctx, l, p, gw)
	}
	return []*action{
		{
			name:           "controller-restart",
			weight:         3,
			holdMin:        10 * time.Second,
			holdMax:        30 * time.Second,
			recoveryBudget: 90 * time.Second,
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				return l.stopController(ctx, gw)
			},
			restore: func(ctx context.Context, l *lab, gw string) error {
				return l.startController(ctx, gw)
			},
		},
		{
			name:           "gateway-kill",
			weight:         1,
			holdMin:        15 * time.Second,
			holdMax:        45 * time.Second,
			recoveryBudget: 180 * time.Second,
			inject:         injectGatewayKill,
			restore:        startAndRestore,
		},
		{
			name:           "agent-terminate",
			weight:         2,
			holdMin:        10 * time.Second,
			holdMax:        30 * time.Second,
			recoveryBudget: 180 * time.Second,
			inject:         injectAgentTerminate,
			restore:        startAndRestore,
		},
		{
			name:   "gateway-restart",
			weight: 2,
			// No hold: `docker restart` is the fault and its own undo.
			holdMin:        0,
			holdMax:        0,
			recoveryBudget: 180 * time.Second,
			inject: func(ctx context.Context, l *lab, gw string, _ int) error {
				return l.restartGateway(ctx, gw)
			},
			restore: restore,
		},
	}
}

// allActions is the registry the runner drives: the starter faults plus
// the configuration change, appended — never inserted — so a recorded
// seed still replays the sequence it recorded.
//
// config-flip is the operational fault the other four cannot express: a
// gateway is reconfigured and restarted onto the new configuration while
// the lab is under load, the way a rollout, a tuning change or an
// emergency flag flip actually reaches production. Its restart is the
// fault and its own undo, so like gateway-restart it holds nothing and
// pays the full container-lifecycle recovery budget.
func allActions(p *profile, ap *applier) []*action {
	return append(starterActions(p), &action{
		name:           "config-flip",
		weight:         2,
		holdMin:        0,
		holdMax:        0,
		recoveryBudget: 180 * time.Second,
		usesFlip:       true,
		applicable:     ap.applicable,
		inject: func(ctx context.Context, _ *lab, gw string, flip int) error {
			return ap.flip(ctx, gw, flip)
		},
		restore: func(ctx context.Context, l *lab, gw string) error {
			return restoreNode(ctx, l, p, gw)
		},
	})
}

// injectGatewayKill hard-kills the container. containerlab deploys with
// `restart: always`, so the policy is flipped off first — otherwise
// docker revives the node before the fault is observable at all.
func injectGatewayKill(ctx context.Context, l *lab, gw string, _ int) error {
	if err := l.setRestartPolicy(ctx, gw, "no"); err != nil {
		return err
	}
	return l.killGateway(ctx, gw)
}

// injectAgentTerminate signals the agent (PID 1) and waits for the container
// to follow it down. Whether the agent drains its gateway chassis before
// exiting depends on how the lab was deployed — the fault is the same
// either way, and the drain only changes how much the run loses.
func injectAgentTerminate(ctx context.Context, l *lab, gw string, _ int) error {
	if err := l.setRestartPolicy(ctx, gw, "no"); err != nil {
		return err
	}
	if err := l.terminateAgent(ctx, gw); err != nil {
		return err
	}
	return l.waitContainerExit(ctx, gw, agentExitTimeout)
}

// startAndRestoreGateway brings a killed container back: start it,
// restore the restart policy containerlab set, then put the node back in
// service (underlay veth, BGP, node-local workloads).
//
// The policy is put back even when the start failed: the inject pinned it
// to "no" so docker would not revive the node mid-fault, and leaving it
// there hands the lab a gateway docker will never bring back up.
func startAndRestoreGateway(ctx context.Context, l *lab, p *profile, gw string) error {
	startErr := l.startGateway(ctx, gw)
	policyErr := l.setRestartPolicy(ctx, gw, "always")
	if startErr != nil {
		return startErr
	}
	if policyErr != nil {
		return policyErr
	}
	return restoreNode(ctx, l, p, gw)
}
