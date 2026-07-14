package main

import (
	"context"
	"fmt"
	"time"
)

// profileApplyTimeout is how long a reconfigured gateway gets to come
// back before the run is abandoned. It matches the recovery budget of the
// actions that recycle a container: the mechanic is the same `docker
// restart`, and the node has to be all the way back before the roll moves
// on to the next one.
const profileApplyTimeout = 180 * time.Second

// applier owns the agent configuration of every gateway for the length of
// a run.
//
// Applying a profile to a running lab needs no image rebuild and no
// redeploy: the config file is the only thing that changes, and the
// gwnode entrypoint execs the agent, so `docker restart` is the reload.
// What makes that safe is the order — render every gateway's config and
// hand each one to the agent's own --check-config *before* a single live
// file is touched, then roll the gateways one at a time so the lab keeps
// forwarding while it is reconfigured.
//
// The same path serves the config-flip action mid-run: toggle a
// whitelisted option, validate, swap, restart.
type applier struct {
	lab     *lab
	profile *profile

	// jrnl records every configuration the applier puts on a gateway. It is
	// set once the run has earned an artifact directory: the applier itself
	// is built earlier, because the action registry binds config-flip to it
	// and the -weights check runs against that registry — ahead of
	// everything that creates a file.
	jrnl *journal

	// base is the host-side gwnode config — the document every overlay is
	// layered over, and byte-for-byte what a fresh gateway already has.
	base []byte

	// mgmtIP is each gateway's management address: the backend its API
	// VIP forwards to.
	mgmtIP map[string]string

	// baseline is the configuration the profile put on each gateway, and
	// current is what the gateway is running now. A flip toggles between
	// the two — "back to the base" means back to what the profile
	// configured, not back to what the image ships.
	baseline map[string]map[string]any
	current  map[string]map[string]any
}

func newApplier(l *lab, p *profile, base []byte) *applier {
	return &applier{
		lab:      l,
		profile:  p,
		base:     base,
		mgmtIP:   map[string]string{},
		baseline: map[string]map[string]any{},
		current:  map[string]map[string]any{},
	}
}

// discover reads the per-gateway facts a rendered config needs.
func (a *applier) discover(ctx context.Context) error {
	for _, gw := range gatewayNames() {
		ip, err := a.lab.mgmtIP(ctx, gw)
		if err != nil {
			return err
		}
		a.mgmtIP[gw] = ip
	}
	return nil
}

// applyProfile puts the profile's configuration on every gateway.
//
// Validation is a separate pass over all three gateways, and it runs to
// completion before the first live file is swapped: a profile the agent
// rejects on one gateway must not leave the lab half-reconfigured, with
// two gateways rolled onto it and the third refusing to start.
func (a *applier) applyProfile(ctx context.Context) error {
	rendered := make(map[string][]byte, len(gatewayNames()))
	for _, gw := range gatewayNames() {
		raw, err := renderConfig(a.base, a.profile.gwConfig(gw), a.mgmtIP[gw])
		if err != nil {
			return fmt.Errorf("profile %s on %s: %w", a.profile.name, gw, err)
		}
		valid, detail, err := a.stage(ctx, gw, raw)
		if err != nil {
			return err
		}
		if !valid {
			return fmt.Errorf("the agent rejected the %s configuration for %s: %s",
				a.profile.name, gw, detail)
		}
		rendered[gw] = raw
	}

	for _, gw := range gatewayNames() {
		// Two independent parses: a flip mutates `current` in place, and
		// `baseline` is what it toggles back to.
		for _, into := range []map[string]map[string]any{a.baseline, a.current} {
			doc, err := parseConfig(rendered[gw])
			if err != nil {
				return err
			}
			into[gw] = doc
		}

		applied, err := a.swap(ctx, gw, rendered[gw], !a.profile.gwConfig(gw).empty())
		if err != nil {
			return err
		}
		detail := fmt.Sprintf("already on the %s configuration", a.profile.name)
		if applied {
			detail = fmt.Sprintf("restarted onto the %s configuration", a.profile.name)
		}
		a.jrnl.emit(event{
			Event: evProfileApply, Target: gw,
			Executed: boolPtr(applied), Detail: detail,
		})
	}
	return nil
}

// stage writes a rendered config next to the live one and hands it to the
// agent's own validator. Nothing live has been touched when it returns.
func (a *applier) stage(ctx context.Context, gw string, raw []byte) (bool, string, error) {
	if err := a.lab.writeFile(ctx, gw, agentConfigNextPath, string(raw)); err != nil {
		return false, "", err
	}
	return a.lab.checkConfig(ctx, gw, agentConfigNextPath)
}

// swap puts a staged config live and restarts the gateway onto it,
// reporting whether it had to do anything at all.
//
// A gateway already running the rendered config is left alone — on a
// fresh lab the default profile renders exactly the baked bytes, so it
// restarts nothing and costs the run no re-election.
func (a *applier) swap(ctx context.Context, gw string, raw []byte, owned bool) (bool, error) {
	live, err := a.lab.readFile(ctx, gw, agentConfigPath)
	if err != nil {
		return false, err
	}
	if err := a.mark(ctx, gw, owned); err != nil {
		return false, err
	}
	if live == string(raw) {
		return false, nil
	}
	if err := a.lab.moveFile(ctx, gw, agentConfigNextPath, agentConfigPath); err != nil {
		return false, err
	}
	if err := a.lab.restartGateway(ctx, gw); err != nil {
		return false, err
	}
	if err := restoreNode(ctx, a.lab, a.profile, gw); err != nil {
		return false, err
	}
	return true, a.waitBack(ctx, gw)
}

// mark writes — or removes — the marker the gwnode entrypoint reads.
//
// topology.clab.yml sets OVN_NETWORK_DRAIN_ON_SHUTDOWN on every gateway,
// and the agent's environment layer beats its config file, so a profile
// that enables the drain would never see it take effect. The marker tells
// the entrypoint that a chaos profile owns the whole file and the
// deploy-time override has to step aside. A gateway back on the baked
// config has the marker removed, so the lab behaves as deployed again.
func (a *applier) mark(ctx context.Context, gw string, owned bool) error {
	if !owned {
		return a.lab.removeFile(ctx, gw, profileMarkerPath)
	}
	return a.lab.writeFile(ctx, gw, profileMarkerPath, a.profile.name)
}

// waitBack holds the roll until the reconfigured gateway is back: the
// container healthy (the image's healthcheck covers OVS, ovn-controller
// and the agent) and its chassis re-registered in SB. Rolling on before
// that would have two of three gateways down at once.
func (a *applier) waitBack(ctx context.Context, gw string) error {
	deadline := a.lab.now().Add(profileApplyTimeout)
	for a.lab.now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a.lab.containerHealth(ctx, gw) == "healthy" && a.lab.chassisInSB(ctx, gw) {
			return nil
		}
		a.lab.sleep(convergePollInterval)
	}
	return fmt.Errorf("%s did not come back within %s after its configuration was swapped",
		gw, profileApplyTimeout)
}

// applicable reports whether a drawn flip means anything on a gateway's
// current configuration — the masquerade flip needs a VIP to flip it on,
// the CIDR flip a baseline to toggle back to. The engine's guardrails
// consult it, so an inapplicable flip is a journaled skip rather than a
// rewrite and a restart that change nothing.
func (a *applier) applicable(gw string, idx int) bool {
	c, ok := a.flipCtx(gw)
	return ok && flips()[idx].applicable(c)
}

// flip rewrites one gateway's configuration mid-run: toggle a whitelisted
// option, validate the result the way applyProfile does, and only then
// swap the file and restart the node onto it — a rolling reconfiguration
// under fault load.
//
// A configuration the agent rejects is journaled and dropped, and the
// tracker goes back to what the gateway is still running. That is not a
// failure of the run: it is the answer an operator gets from a rejected
// rollout, and the whitelist is deliberately free to draw a combination
// the agent must refuse rather than accept.
func (a *applier) flip(ctx context.Context, gw string, idx int) error {
	f := flips()[idx]
	c, ok := a.flipCtx(gw)
	if !ok {
		return fmt.Errorf("no configuration is tracked for %s", gw)
	}

	before, err := marshalConfig(c.doc)
	if err != nil {
		return err
	}
	from, to := f.apply(c)
	raw, err := marshalConfig(c.doc)
	if err != nil {
		return err
	}

	valid, detail, err := a.stage(ctx, gw, raw)
	if err != nil {
		return err
	}
	if !valid {
		reverted, err := parseConfig(before)
		if err != nil {
			return err
		}
		a.current[gw] = reverted
		a.jrnl.emit(event{
			Event: evConfigFlip, Target: gw, Flip: f.name, From: from, To: to,
			Rejected: boolPtr(true), Detail: detail,
		})
		return nil
	}

	// The marker goes down before the restart: from here on the file is
	// authoritative, and the deploy-time drain override must not beat a
	// drain the flip has just written into it.
	if err := a.mark(ctx, gw, true); err != nil {
		return err
	}
	if err := a.lab.moveFile(ctx, gw, agentConfigNextPath, agentConfigPath); err != nil {
		return err
	}
	a.jrnl.emit(event{
		Event: evConfigFlip, Target: gw, Flip: f.name, From: from, To: to,
		Rejected: boolPtr(false),
	})
	return a.lab.restartGateway(ctx, gw)
}

func (a *applier) flipCtx(gw string) (flipCtx, bool) {
	doc, ok := a.current[gw]
	if !ok {
		return flipCtx{}, false
	}
	return flipCtx{doc: doc, baseline: a.baseline[gw], mgmtIP: a.mgmtIP[gw]}, true
}
