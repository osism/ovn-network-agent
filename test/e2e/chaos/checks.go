package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// checkInterval is the cadence of the baseline sweep. It runs for the
// whole run, alongside the prober and independently of what the engine
// is doing, so an invariant broken during a fault hold is caught.
const checkInterval = 10 * time.Second

// baselineChecks are the invariants that must hold across the whole run,
// no matter which faults the engine picked:
//
//   - agent-alive: every node the engine considers healthy runs an agent.
//     Nodes under fault are skipped — having no agent is the fault.
//   - dual-claim: no chassisredirect port is claimed by two chassis at
//     once. A split gateway port forwards the same traffic twice and is
//     the failure mode HA re-election exists to avoid.
//
// Reachability recovery after each action is the engine's own gate (see
// converge), because only it knows the action's budget.
type baselineChecks struct {
	lab    *lab
	engine *engine

	// counts is what the sweeps actually evaluated. Only the sweep
	// goroutine touches it; drive reads it after joining that goroutine.
	counts checkCounts
}

// run sweeps until ctx is cancelled. The first sweep runs immediately, so
// even a run shorter than the sweep interval evaluates the invariants
// once — finalize now holds the run to having done so.
func (c *baselineChecks) run(ctx context.Context) {
	c.sweep(ctx)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweep(ctx)
		}
	}
}

func (c *baselineChecks) sweep(ctx context.Context) {
	c.counts.Sweeps++
	c.checkAgentsAlive(ctx)
	c.checkNoDualClaim(ctx)
}

// finalize records what the sweeps evaluated, and fails the run when the
// dual-claim invariant was never evaluated at all.
//
// It is the single most important thing this harness asserts, and every
// one of its sweeps can fail for reasons that have nothing to do with the
// lab — central under load, the sbctl --timeout=5 expiring. A run whose
// every sweep failed that way checked nothing, and without this it would
// report zero violations and a clean pass, with the check-error lines
// that say otherwise buried in a ten-minute journal nobody reads on green.
func (c *baselineChecks) finalize() {
	c.engine.recordChecks(c.counts)
	if c.counts.DualClaim > 0 {
		return
	}
	c.engine.violate(violationRecord{
		Kind: violationChecksNeverRan,
		Detail: fmt.Sprintf("the dual-claim invariant was never evaluated: %d sweeps, %d check errors",
			c.counts.Sweeps, c.counts.Errors),
	})
}

// checkError records a check the runner could not answer. It is a
// runner-side problem, not a broken lab invariant — but the invariant
// went unevaluated, so it is counted (finalize holds the run to the
// count) and journaled rather than swallowed. A cancelled run is not a
// failed check: every in-flight command fails on ctx.Done() by design.
func (c *baselineChecks) checkError(ctx context.Context, kind string, err error) {
	if ctx.Err() != nil {
		return
	}
	c.counts.Errors++
	c.engine.jrnl.emit(event{Event: evCheckError, Kind: kind, Detail: err.Error()})
}

func (c *baselineChecks) checkAgentsAlive(ctx context.Context) {
	for _, gw := range c.engine.healthyNodes() {
		if ctx.Err() != nil {
			return
		}
		alive, err := c.lab.agentAlive(ctx, gw)
		if err != nil {
			// The question could not be asked — a docker daemon under load,
			// an expired command timeout. Reading that as "no agent" would
			// fail the run over a node that is perfectly healthy.
			c.checkError(ctx, violationAgentDown, err)
			continue
		}
		if alive {
			continue
		}
		// Re-read the node state: the engine may have started a fault on
		// this node between the snapshot and the probe, in which case a
		// dead agent is expected, not a violation.
		if c.engine.nodeState(gw) != nodeHealthy {
			continue
		}
		c.engine.violate(violationRecord{
			Kind: violationAgentDown, Target: gw,
			Detail: "no ovn-network-agent process on a node the run considers healthy",
		})
	}
}

func (c *baselineChecks) checkNoDualClaim(ctx context.Context) {
	claims, err := c.lab.crPortClaims(ctx)
	if err != nil {
		c.checkError(ctx, violationDualClaim, err)
		return
	}
	c.counts.DualClaim++
	for _, claim := range claims {
		if len(claim.chassis) < 2 {
			continue
		}
		names := make([]string, 0, len(claim.chassis))
		for _, uuid := range claim.chassis {
			names = append(names, c.lab.chassisName(ctx, uuid))
		}
		c.engine.violate(violationRecord{
			Kind: violationDualClaim, Target: claim.port,
			Detail: fmt.Sprintf("%s claimed by %d chassis at once: %s",
				claim.port, len(names), strings.Join(names, ", ")),
		})
	}
}
