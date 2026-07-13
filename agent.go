package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Agent is the main OVN route synchronization agent.
type Agent struct {
	cfg Config
	// ovn is the OVN database client. It is nil in port-forward-only mode
	// (cfg.PortForwardOnly), where the agent runs as a standalone VIP
	// service without any OVN connection.
	ovn     *OVNClient
	routing *RouteManager

	// Channel to trigger reconciliation
	reconcileCh chan struct{}

	// effectiveFilters holds the network filters in effect for the current
	// reconciliation cycle — either from manual config or auto-discovered
	// from OVN Logical_Router_Port.Networks.
	effectiveFilters []*net.IPNet

	// consecutiveReAdds tracks how many reconcile cycles in a row the
	// post-change verification had to re-add missing routes. A sustained
	// non-zero value indicates persistent route instability (e.g. FRR
	// misconfiguration) and triggers escalated logging.
	consecutiveReAdds int

	// missingChassis tracks when each chassis was first observed as absent
	// from the OVN SB Chassis table. Used for stale entry cleanup with a
	// configurable grace period.
	missingChassis map[string]time.Time

	// staleCleanupJitter is a random duration (0-30s) added to the grace
	// period to prevent multiple agents from cleaning up simultaneously.
	staleCleanupJitter time.Duration
}

// maxStaleCleanupJitter is the maximum random jitter added to the stale
// chassis grace period to prevent thundering-herd cleanup across agents.
const maxStaleCleanupJitter = 30 * time.Second

// ovnConnectRetryInterval is how long Run waits between failed OVN connect
// attempts. The agent is a long-running daemon: when its OVN endpoint is
// unreachable on cold start, the right behaviour is to keep retrying rather
// than crash a unit that systemd would only restart in a tight loop anyway.
const ovnConnectRetryInterval = 5 * time.Second

func NewAgent(cfg Config) (*Agent, error) {
	a := &Agent{
		cfg:                cfg,
		routing:            NewRouteManager(cfg),
		reconcileCh:        make(chan struct{}, 1),
		missingChassis:     make(map[string]time.Time),
		staleCleanupJitter: time.Duration(rand.Int63n(int64(maxStaleCleanupJitter))),
	}

	// In port-forward-only mode there is no OVN connection; a.ovn stays nil
	// and every OVN-dependent step is gated on cfg.PortForwardOnly.
	if !cfg.PortForwardOnly {
		a.ovn = NewOVNClient(cfg, a.triggerReconcile)
	}

	return a, nil
}

// triggerReconcile requests an asynchronous reconciliation (non-blocking).
func (a *Agent) triggerReconcile() {
	select {
	case a.reconcileCh <- struct{}{}:
	default:
		// Already pending
	}
}

// connectWithRetry calls connect repeatedly, sleeping retryInterval between
// failed attempts, until connect returns nil or ctx is cancelled. It returns
// nil on success or ctx.Err() if the context was cancelled during a retry
// wait. Errors from connect itself do not terminate the loop — the agent is
// a long-running daemon and the contract on cold start with an unreachable
// OVN endpoint is to keep retrying, not exit.
func connectWithRetry(ctx context.Context, connect func(context.Context) error, retryInterval time.Duration) error {
	for {
		err := connect(ctx)
		if err == nil {
			return nil
		}
		slog.Error("failed to connect to OVN, retrying", "error", err, "retry_in", retryInterval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

// Run starts the agent: connects to OVN, runs initial reconciliation,
// then loops on events and periodic reconciliation.
func (a *Agent) Run(ctx context.Context) error {
	// Bridge setup exists for FIP ARP resolution and is OVN-gateway
	// specific. In port-forward-only mode the node need not have br-ex, so
	// the bridge device, its link-local IP and proxy ARP are all skipped.
	if !a.cfg.PortForwardOnly {
		// Gateway mode announces Floating IP routes to BGP through vtysh, so
		// warn once at startup if FRR is not reachable (see warnIfVtyshMissing).
		// Port-forward-only mode never touches vtysh, so this is skipped there.
		warnIfVtyshMissing(exec.LookPath)

		// Verify that the bridge device exists and is up before proceeding.
		if err := a.routing.CheckBridgeDevice(); err != nil {
			return fmt.Errorf("bridge device check failed: %w", err)
		}

		// Add a link-local IP to br-ex so the kernel can ARP on the interface.
		if err := a.routing.EnsureBridgeIP(a.cfg.BridgeIP); err != nil {
			return fmt.Errorf("ensure bridge IP: %w", err)
		}

		// Enable proxy ARP so the kernel responds to ARP requests for FIP addresses.
		if err := a.routing.EnableProxyARP(); err != nil {
			return fmt.Errorf("enable proxy ARP: %w", err)
		}
	}

	// Set up veth VRF leak for route leaking between default VRF and provider VRF.
	if err := a.routing.SetupVethLeak(); err != nil {
		return fmt.Errorf("veth VRF leak setup: %w", err)
	}

	// Set up port forwarding (DNAT) rules (requires veth pair).
	if err := a.routing.SetupPortForward(); err != nil {
		return fmt.Errorf("port forward setup: %w", err)
	}

	if a.cfg.PortForwardOnly {
		slog.Info("port-forward-only mode: serving configured VIPs without an OVN connection")
	} else {
		if a.cfg.GatewayPort == "" {
			slog.Info("tracking all chassisredirect ports (multi-router mode)")
		} else {
			slog.Info("tracking single chassisredirect port", "gateway_port", a.cfg.GatewayPort)
		}

		// Connect to OVN with retry.
		if err := connectWithRetry(ctx, a.ovn.Connect, ovnConnectRetryInterval); err != nil {
			return err
		}
		defer a.ovn.Close()

		// Restore any gateway chassis priorities that were drained by a previous run.
		if a.cfg.DrainOnShutdown {
			a.ovn.RestoreDrainedGateways(ctx, a.ovn.GetState().LocalChassisName)
		}
	}

	// Initial reconciliation
	a.reconcile(ctx, "startup")

	// Drain any reconcile signals queued during startup — the initial
	// reconcile already handled the current state.
	select {
	case <-a.reconcileCh:
	default:
	}

	// Main loop
	ticker := time.NewTicker(a.cfg.ReconcileInterval)
	defer ticker.Stop()

	slog.Info("agent running", "reconcile_interval", a.cfg.ReconcileInterval)

	for {
		select {
		case <-ctx.Done():
			// Wait for the OVN refresh loop (which shares the now-cancelled
			// ctx) to fully exit before the shutdown path touches o.state, so
			// a still-in-flight loop refresh cannot interleave with the
			// post-drain refresh below. This makes the shutdown path the sole
			// writer of o.state.
			if !a.cfg.PortForwardOnly {
				a.ovn.waitRefreshLoopStopped()
			}
			// Drain must happen BEFORE cleanup and BEFORE OVN connection close.
			// Use a fresh context since the parent ctx is already cancelled.
			// Drain is OVN-specific and is skipped in port-forward-only mode.
			if !a.cfg.PortForwardOnly && a.cfg.DrainOnShutdown {
				drainCtx, drainCancel := context.WithTimeout(context.Background(), a.cfg.DrainTimeout)
				slog.Info("drain mode active, migrating gateways away", "timeout", a.cfg.DrainTimeout)
				drainStart := time.Now()
				err := a.ovn.DrainGateways(drainCtx, a.ovn.GetState().LocalChassisName)
				drainElapsed := time.Since(drainStart)
				switch {
				case err != nil:
					slog.Error("drain failed", "error", err)
					recordDrain("error", drainElapsed)
				case drainCtx.Err() != nil:
					recordDrain("timeout", drainElapsed)
				default:
					recordDrain("completed", drainElapsed)
				}
				drainCancel()

				// The OVN refresh loop stopped the moment ctx was cancelled,
				// so o.state has been frozen since before the drain — it still
				// lists the routers that have just migrated away. Refresh it
				// once now, with a fresh context, so the RemoveManagedNBEntries
				// call inside cleanup() sees the post-drain reality and does
				// not delete the default routes and MAC bindings that the
				// chassis taking over now depends on.
				refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 10*time.Second)
				a.ovn.refreshState(refreshCtx)
				refreshCancel()
			}
			if a.cfg.CleanupOnShutdown {
				slog.Info("shutting down, cleaning up routes")
				a.cleanup()
			} else {
				slog.Info("shutting down, keeping routes in place")
			}
			return nil

		case <-a.reconcileCh:
			slog.Debug("event-triggered reconciliation")
			a.reconcile(ctx, "event")

		case <-ticker.C:
			slog.Debug("periodic reconciliation")
			a.reconcile(ctx, "periodic")
		}
	}
}

// reconcile ensures the local routing state matches the desired state from OVN.
// The trigger label identifies why the cycle ran (event, periodic, startup).
func (a *Agent) reconcile(ctx context.Context, trigger string) {
	start := time.Now()
	setReconcileInProgress(true)
	defer func() {
		setReconcileInProgress(false)
		recordReconcile(trigger, time.Since(start))
	}()

	// In port-forward-only mode there is no OVN client; a zero-value
	// OVNState (no local routers, no SNAT IPs, no discovered networks)
	// drives the rest of the cycle through the port-forward-only path.
	var state OVNState
	if a.ovn != nil {
		state = a.ovn.GetState()
	}

	// Compute effective network filters for this cycle.
	a.effectiveFilters = a.computeEffectiveNetworks(state.DiscoveredNetworks)

	// hairpinTargets maps each IP that needs a hairpin flow to the MAC of
	// the router port that owns it and the localnet segment its external
	// network is on. This includes FIPs, SNAT IPs, and router gateway IPs
	// (LRP networks). Port-forward VIPs are intentionally excluded because
	// their DNAT is handled by nftables.
	//
	// The MAC is used as mod_dl_dst in the hairpin flow so that OVN's
	// L2 lookup delivers the reflected packet to the correct router; the
	// segment selects the patch port the flow binds to.
	hairpinTargets := make(map[string]HairpinTarget, len(state.NATIPToRouterMAC))
	for ip, mac := range state.NATIPToRouterMAC {
		hairpinTargets[ip] = HairpinTarget{RouterMAC: mac, Segment: state.NATIPToSegment[ip]}
	}
	// Router gateway IPs (LRP networks) are included so that VMs on a
	// same-chassis router can reach other routers' gateway addresses,
	// matching the behaviour seen from outside.
	for _, lr := range state.LocalRouters {
		for _, cidr := range lr.LRPNetworks {
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			if lr.LRPMAC != "" {
				hairpinTargets[ip.String()] = HairpinTarget{RouterMAC: lr.LRPMAC, Segment: segmentName(lr.Segment)}
			}
		}
	}

	// desiredSegments is the deduplicated set of localnet segments the
	// locally-active routers need a data plane for. Routers whose segment
	// is unresolved contribute the "" fallback segment.
	segmentSet := make(map[string]DesiredSegment)
	for _, lr := range state.LocalRouters {
		key := segmentName(lr.Segment)
		if _, ok := segmentSet[key]; ok {
			continue
		}
		d := DesiredSegment{LocalnetPort: key}
		if lr.Segment != nil {
			d.VLANTag = lr.Segment.VLANTag
		}
		segmentSet[key] = d
	}
	desiredSegments := make([]DesiredSegment, 0, len(segmentSet))
	for _, d := range segmentSet {
		desiredSegments = append(desiredSegments, d)
	}
	sort.Slice(desiredSegments, func(i, j int) bool {
		return desiredSegments[i].LocalnetPort < desiredSegments[j].LocalnetPort
	})

	// hairpinIPs is the flat list of IPs for kernel routes and FRR — the
	// single choke point where the IPv4-only route/announce plane forks off
	// the deliberately dual-stack hairpinTargets map. Non-IPv4 addresses (IPv6
	// FIPs, SNAT IPs, and LRP gateway IPs) are excluded here: everything
	// downstream (AddKernelRoute, AddFRRRoutes, BGP, the takeover marker) is
	// IPv4-only, so a v6 address would fail the FRR batch and block the marker.
	// The OVS hairpin plane keeps the v6 targets (#54); full IPv6 route support
	// is tracked in #85/#70. Map keys are already unique; just sort.
	hairpinIPs := make([]string, 0, len(hairpinTargets))
	var excludedNonV4 []string
	for ip := range hairpinTargets {
		if parsed := net.ParseIP(ip); parsed == nil || parsed.To4() == nil {
			excludedNonV4 = append(excludedNonV4, ip)
			continue
		}
		hairpinIPs = append(hairpinIPs, ip)
	}
	sort.Strings(hairpinIPs)
	if len(excludedNonV4) > 0 {
		sort.Strings(excludedNonV4)
		slog.Debug("excluding non-IPv4 addresses from the route/announce plane, IPv6 support tracked in #85/#70",
			"ips", excludedNonV4)
	}

	// desiredIPs extends hairpinIPs with port-forward VIPs — these need
	// kernel routes on br-ex and FRR static routes for BGP announcement,
	// independent of whether any OVN routers are locally active.
	desiredIPs := hairpinIPs
	for _, pf := range a.cfg.PortForwards {
		desiredIPs = append(desiredIPs, pf.VIP)
	}
	desiredIPs = uniqueIPs(desiredIPs)

	setDesiredState(len(desiredIPs), len(state.LocalRouters), len(a.effectiveFilters))
	setLocalnetSegments(len(desiredSegments))

	slog.Info("reconciling",
		"has_local_routers", state.HasLocalRouters,
		"local_routers", len(state.LocalRouters),
		"local_host", state.LocalChassisName,
		"desired_ips", len(desiredIPs),
		"effective_networks", len(a.effectiveFilters),
	)
	if len(desiredIPs) > 0 {
		slog.Debug("desired IP list", "ips", desiredIPs)
	}

	switch {
	case a.cfg.PortForwardOnly:
		// Port-forward-only mode: no OVN state to act on. OVS flows,
		// gateway routing, and veth-leak/prefix-list network
		// reconciliation are all OVN-driven and skipped entirely — only
		// port forwarding and the VIP routes below are managed.
	case state.HasLocalRouters:
		// Ensure per-segment OVS MAC-tweak flows and kernel interfaces are
		// in place (only when active).
		if err := a.routing.EnsureSegments(desiredSegments); err != nil {
			slog.Error("failed to ensure OVS flows", "error", err)
		}

		// Reconcile per-IP hairpin flows for same-chassis cross-router
		// communication. These reflect FIP/SNAT-IP traffic from OVN back
		// into OVN's external pipeline instead of sending it to the kernel,
		// fixing the case where two routers share the same gateway chassis.
		if err := a.routing.ReconcileOVSHairpinFlows(hairpinTargets); err != nil {
			slog.Error("failed to reconcile OVS hairpin flows", "error", err)
		}

		// Ensure OVN default routes and static MAC bindings for local
		// routers, each pointing at the MAC of its own segment's kernel
		// interface. When the segment bindings are not (yet) resolved,
		// fall back to the bridge MAC — the flat single-network value.
		macByLRP := make(map[string]string, len(state.LocalRouters))
		fallbackMAC := ""
		for _, lr := range state.LocalRouters {
			mac := a.routing.SegmentMAC(segmentName(lr.Segment))
			if mac == "" {
				if fallbackMAC == "" {
					if m, err := a.routing.GetBridgeMAC(); err == nil {
						fallbackMAC = m.String()
					}
				}
				mac = fallbackMAC
			}
			macByLRP[lr.LRPName] = mac
		}
		if err := a.ovn.EnsureGatewayRouting(ctx, state.LocalRouters, macByLRP); err != nil {
			slog.Error("failed to ensure gateway routing", "error", err)
		}

		// Ensure the active chassis has a strictly higher priority than
		// standby peers, preventing reverse failover after drain/restore.
		if err := a.ovn.EnsureActivePriorityLead(ctx, state.LocalRouters, state.LocalChassisName); err != nil {
			slog.Error("failed to ensure active priority lead", "error", err)
		}

		// Reconcile per-network veth leak routes and policy rules.
		if err := a.routing.ReconcileVethLeakNetworks(a.effectiveFilters); err != nil {
			slog.Error("failed to reconcile veth leak networks", "error", err)
		}

		// Reconcile FRR prefix-list entries for discovered networks.
		if err := a.routing.ReconcileFRRPrefixList(a.effectiveFilters); err != nil {
			slog.Error("failed to reconcile FRR prefix-list", "error", err)
		}
	default:
		// No locally active routers — remove per-network veth leak and prefix-list entries.
		if err := a.routing.ReconcileVethLeakNetworks(nil); err != nil {
			slog.Error("failed to clean veth leak networks", "error", err)
		}
		if err := a.routing.ReconcileFRRPrefixList(nil); err != nil {
			slog.Error("failed to clean FRR prefix-list", "error", err)
		}
		if err := a.routing.ReconcileOVSHairpinFlows(nil); err != nil {
			slog.Error("failed to clean OVS hairpin flows", "error", err)
		}
	}

	// Port forwarding reconciliation runs regardless of local router
	// presence — DNAT VIPs are managed independently of OVN gateway state.
	if err := a.routing.ReconcilePortForward(a.effectiveFilters, state.SNATIPs); err != nil {
		slog.Error("failed to reconcile port forwarding", "error", err)
	}

	// ipDev maps each desired IP to the kernel interface its /32 route
	// belongs on: the owning segment's interface for FIPs, SNAT IPs, and
	// router gateway IPs. Port-forward VIPs are absent and default to the
	// bridge device. Computed after EnsureSegments so the bindings are
	// fresh for this cycle.
	//
	// skipKernelRoute holds IPs whose VLAN segment could not be resolved this
	// cycle. EnsureSegments leaves such a segment without a binding (a
	// transient discovery failure nulls the whole map, or a per-segment
	// interface setup failed), so SegmentDev would report the untagged
	// provider bridge. Reconciling the /32 onto that fallback would atomically
	// move a VLAN FIP/SNAT route off its subinterface and mis-deliver the
	// traffic, so those IPs are skipped and their existing route is left in
	// place until the segment resolves again. Flat segments legitimately route
	// on the bridge and are never skipped.
	ipDev := make(map[string]string, len(hairpinTargets))
	skipKernelRoute := make(map[string]bool)
	for ip, target := range hairpinTargets {
		if a.segmentRouteUnresolved(target.Segment, segmentSet) {
			skipKernelRoute[ip] = true
			continue
		}
		ipDev[ip] = a.routing.SegmentDev(target.Segment)
	}

	// Ensure routes for all desired IPs (FIPs, SNATs, and port forward VIPs).
	// When no local routers are present but port forwards are configured,
	// this still installs VIP routes on br-ex and in FRR.
	announced := false
	if len(desiredIPs) > 0 || state.HasLocalRouters {
		announced = a.ensureRoutes(desiredIPs, ipDev, skipKernelRoute)
	} else {
		a.removeAllRoutes("no locally active routers and no port forward VIPs")
	}

	// On a node that owns local routers, stamp the takeover readiness marker on
	// each managed default route once the announce succeeded. A draining peer
	// polls NB for this marker before it releases its own FIP routes, so it
	// must only be written when this node can actually forward (ensureRoutes
	// returns true only after RefreshBGP succeeded). HasLocalRouters is false in
	// port-forward-only mode (a.ovn is nil), so this is skipped there.
	if announced && state.HasLocalRouters {
		if err := a.ovn.MarkTakeoverReady(ctx, state.LocalRouters); err != nil {
			slog.Error("failed to write takeover readiness marker", "error", err)
		}
	}

	// Surface desired routes that are configured in FRR but not actually
	// advertised via BGP — ensureRoutes alone cannot detect this.
	a.checkFRRRouteActivity(desiredIPs)

	// Check for stale chassis entries from dead nodes (runs on every agent).
	// This runs after gateway routing reconciliation so that a surviving agent
	// creates its own routes before removing entries from dead chassis.
	// Skipped in port-forward-only mode: there is no OVN SB Chassis table.
	if !a.cfg.PortForwardOnly {
		a.cleanupStaleChassis(ctx, state.AllChassisNames)
	}
}

// cleanupStaleChassis detects chassis that have disappeared from the SB Chassis
// table and, after a configurable grace period (plus random jitter), removes
// their managed NB entries. Any surviving agent can perform this cleanup.
func (a *Agent) cleanupStaleChassis(ctx context.Context, allChassis map[string]bool) {
	if a.cfg.StaleChassisGracePeriod <= 0 {
		return
	}

	referencedChassis := a.ovn.ListManagedRouteChassis(ctx)
	if referencedChassis == nil {
		return
	}

	now := time.Now()
	effectiveGrace := a.cfg.StaleChassisGracePeriod + a.staleCleanupJitter

	// Update missingChassis map: add newly missing, remove returned.
	for chassisName := range referencedChassis {
		if allChassis[chassisName] {
			// Chassis is back (or still alive) — remove from missing tracker.
			if _, tracked := a.missingChassis[chassisName]; tracked {
				slog.Info("previously missing chassis has returned", "chassis", chassisName)
				delete(a.missingChassis, chassisName)
			}
		} else {
			// Chassis is missing.
			if _, tracked := a.missingChassis[chassisName]; !tracked {
				a.missingChassis[chassisName] = now
				slog.Warn("chassis referenced by managed routes is missing from SB",
					"chassis", chassisName)
			}
		}
	}

	// Prune entries for chassis no longer referenced by any managed route.
	for chassisName := range a.missingChassis {
		if !referencedChassis[chassisName] {
			delete(a.missingChassis, chassisName)
		}
	}

	setMissingChassis(len(a.missingChassis))

	// Find chassis that have exceeded the grace period.
	staleChassis := make(map[string]bool)
	for chassisName, firstSeen := range a.missingChassis {
		if now.Sub(firstSeen) >= effectiveGrace {
			staleChassis[chassisName] = true
			slog.Warn("chassis exceeded stale grace period, cleaning up managed entries",
				"chassis", chassisName,
				"missing_since", firstSeen,
				"grace_period", effectiveGrace)
		}
	}

	if len(staleChassis) == 0 {
		return
	}

	if err := a.ovn.CleanupStaleChassisManagedEntries(ctx, staleChassis); err != nil {
		slog.Error("failed to clean up stale chassis entries", "error", err)
		recordStaleChassisCleanup("error", len(staleChassis))
		return
	}

	recordStaleChassisCleanup("success", len(staleChassis))
	for chassisName := range staleChassis {
		delete(a.missingChassis, chassisName)
	}
	setMissingChassis(len(a.missingChassis))
}

// computeEffectiveNetworks returns the network filters to use: manual config if
// set, otherwise auto-discovered networks from OVN Logical_Router_Port. Only
// IPv4 networks are returned — a.effectiveFilters feeds IPv4-only planes
// (ReconcileVethLeakNetworks, ReconcileFRRPrefixList, and the "table ip"
// provider-network rules in ReconcilePortForward), so a v6 network discovered
// from a dual-stack LRP would otherwise abort those loops mid-way or fail the
// whole nft ruleset load. Full IPv6 support is tracked in #85/#70.
func (a *Agent) computeEffectiveNetworks(discovered []*net.IPNet) []*net.IPNet {
	eff := effectiveNetworkFilters(a.cfg.NetworkFilters, discovered)
	v4 := make([]*net.IPNet, 0, len(eff))
	for _, n := range eff {
		if n.IP.To4() != nil {
			v4 = append(v4, n)
		}
	}
	return v4
}

// segmentRouteUnresolved reports whether an IP on the given localnet segment
// must keep its existing kernel /32 route this cycle instead of being routed
// via SegmentDev. It is true only for a VLAN segment (one the desired set
// tagged) whose per-segment binding did not resolve — the case where SegmentDev
// would wrongly return the untagged provider bridge and atomically relocate the
// FIP/SNAT route off its subinterface. Flat segments (bridge is correct),
// resolved VLAN segments, and unknown segments return false and route normally.
func (a *Agent) segmentRouteUnresolved(segment string, desired map[string]DesiredSegment) bool {
	d, ok := desired[segment]
	return ok && d.VLANTag != nil && !a.routing.SegmentResolved(segment)
}

// desiredRouteDev returns the kernel interface an IP's /32 route belongs on,
// defaulting to the provider bridge for IPs without segment information
// (port-forward VIPs, unresolved segments).
func (a *Agent) desiredRouteDev(ip string, ipDev map[string]string) string {
	if dev := ipDev[ip]; dev != "" {
		return dev
	}
	return a.cfg.BridgeDev
}

// ensureRoutes adds routes for all desired IPs and removes stale ones.
// ipDev names the kernel interface each IP's route belongs on; kernel routes
// are reconciled as (IP, device) pairs so a route that sits on the wrong
// segment interface is replaced. IPs in skipKernelRoute keep their existing
// kernel route untouched — their VLAN segment did not resolve this cycle, so
// moving the /32 to the bridge fallback would mis-deliver the traffic.
// ensureRoutes returns whether this node successfully announced the desired
// routes: true unless a kernel-route add, an FRR add, or the BGP soft-refresh
// failed. The drain takeover handshake uses it so a node that attracted BGP
// traffic it cannot yet deliver never signals readiness to a draining peer. A
// steady-state cycle with nothing to change returns true, so a marker missed
// in one cycle self-heals on the next.
func (a *Agent) ensureRoutes(desiredIPs []string, ipDev map[string]string, skipKernelRoute map[string]bool) bool {
	announced := true
	desiredSet := make(map[string]bool, len(desiredIPs))
	for _, ip := range desiredIPs {
		desiredSet[ip] = true
	}

	// Kernel routes live on the provider bridge (br-ex) or a per-VLAN
	// segment interface on it. In port-forward-only mode the node need not
	// have br-ex, so only FRR static routes are managed — the VIP is
	// reachable as a local address on port_forward_dev, and the FRR route
	// handles BGP announcement.
	manageKernel := !a.cfg.PortForwardOnly

	// Collect current state so we only add what is actually missing.
	currentKernelSet := make(map[kernelRouteEntry]bool)
	var currentKernel []kernelRouteEntry
	if manageKernel {
		var err error
		currentKernel, err = a.routing.listKernelRoutes()
		if err != nil {
			slog.Error("failed to list kernel routes", "error", err)
		} else {
			for _, e := range currentKernel {
				currentKernelSet[e] = true
			}
		}
	}

	currentFRRSet := make(map[string]bool)
	currentFRR, err := a.routing.ListFRRRoutes()
	if err != nil {
		slog.Error("failed to list FRR routes", "error", err)
	} else {
		for _, ip := range currentFRR {
			currentFRRSet[ip] = true
		}
	}

	// Collect missing and stale routes, then apply in batches. A kernel
	// route on the wrong device counts as missing: AddKernelRoute uses
	// route replace, which atomically moves the /32 to the right interface
	// (one route per destination and table), so no separate delete is
	// needed for the device change.
	var addFRR []string
	for _, ip := range desiredIPs {
		dev := a.desiredRouteDev(ip, ipDev)
		needsKernel := manageKernel && !skipKernelRoute[ip] && !currentKernelSet[kernelRouteEntry{IP: ip, Dev: dev}]
		needsFRR := !currentFRRSet[ip]

		if !needsKernel && !needsFRR {
			slog.Debug("route already exists", "ip", ip)
			continue
		}

		slog.Info("ensuring route", "ip", ip, "dev", dev, "needs_kernel", needsKernel, "needs_frr", needsFRR)

		if needsKernel {
			if err := a.routing.AddKernelRoute(ip, dev); err != nil {
				slog.Error("failed to add kernel route", "ip", ip, "dev", dev, "error", err)
				announced = false
			}
		}
		if needsFRR {
			addFRR = append(addFRR, ip)
		}
	}

	// Batch-add all missing FRR routes in one vtysh call.
	if len(addFRR) > 0 {
		if err := a.routing.AddFRRRoutes(addFRR); err != nil {
			slog.Error("failed to batch-add FRR routes", "count", len(addFRR), "ips", addFRR, "error", err)
			announced = false
		}
	}

	// Collect stale routes for batch removal. currentKernel/currentFRR are
	// already scoped to agent-owned routes (protocol-tagged kernel routes,
	// veth-nexthop FRR statics), so every listed route that is no longer
	// desired is the agent's own leftover and safe to withdraw. Entries for
	// still-desired IPs on the wrong device were already moved by the route
	// replace above and must not withdraw the FRR announcement.
	var delFRR []string
	var removedKernel []kernelRouteEntry
	removedSet := make(map[string]bool)
	for _, e := range currentKernel {
		if desiredSet[e.IP] {
			continue
		}
		slog.Info("removing stale route", "ip", e.IP, "dev", e.Dev)
		// Remove FRR first to stop attracting traffic before tearing down the data plane.
		if !removedSet[e.IP] {
			delFRR = append(delFRR, e.IP)
			removedSet[e.IP] = true
		}
		removedKernel = append(removedKernel, e)
	}

	// Collect orphaned FRR routes that have no corresponding kernel route.
	for _, ip := range currentFRR {
		if !desiredSet[ip] && !removedSet[ip] {
			slog.Info("removing orphaned FRR route", "ip", ip)
			delFRR = append(delFRR, ip)
		}
	}

	// Batch-remove all stale/orphaned FRR routes in one vtysh call.
	if len(delFRR) > 0 {
		if err := a.routing.DelFRRRoutes(delFRR); err != nil {
			slog.Error("failed to batch-del FRR routes", "count", len(delFRR), "ips", delFRR, "error", err)
		}
	}

	// Remove stale kernel routes (after FRR withdrawal).
	for _, e := range removedKernel {
		if err := a.routing.DelKernelRoute(e.IP, e.Dev); err != nil {
			slog.Error("failed to remove kernel route", "ip", e.IP, "dev", e.Dev, "error", err)
		}
	}

	// Trigger a BGP soft-refresh whenever FRR routes were added or
	// removed. On additions this pushes the new /32s out immediately
	// instead of waiting for FRR's redistribution timing — critical on
	// the takeover chassis during an HA failover, where every extra
	// second of delay is external downtime. "clear ip bgp ... soft out"
	// only re-evaluates outbound policy and re-sends; it never withdraws
	// a route, so re-announcing the existing set alongside the new ones
	// is harmless.
	if len(addFRR) > 0 || len(delFRR) > 0 {
		if err := a.routing.RefreshBGP(); err != nil {
			slog.Warn("BGP soft-refresh failed, peers may wait for MRAI timer", "error", err)
			announced = false
		}
	}

	// Safety net: after any route changes, verify that all desired routes
	// are still present. A BGP soft-refresh re-evaluates outbound policy
	// and could (in edge cases) interact with FRR in unexpected ways.
	if len(addFRR) > 0 || len(delFRR) > 0 {
		a.verifyRoutes(desiredIPs, ipDev, skipKernelRoute)
	}

	return announced
}

// removeAllRoutes removes every agent-owned FIP route. Ownership is scoped by
// the route listings themselves — ListFRRRoutes returns only statics via the
// agent's veth nexthop and ListKernelRoutes only protocol-tagged routes — so
// operator-created routes are never touched.
// The reason parameter is used in log messages to indicate why routes are being removed.
func (a *Agent) removeAllRoutes(reason string) {
	// Collect all agent-owned FRR routes for batch removal (FRR first to stop attracting traffic).
	var delFRR []string
	currentFRR, err := a.routing.ListFRRRoutes()
	if err != nil {
		slog.Error("failed to list FRR routes", "error", err)
	} else {
		delFRR = append(delFRR, currentFRR...)
	}

	if len(delFRR) > 0 {
		slog.Info("batch-removing FRR routes", "count", len(delFRR), "reason", reason)
		if err := a.routing.DelFRRRoutes(delFRR); err != nil {
			slog.Error("failed to batch-del FRR routes", "count", len(delFRR), "ips", delFRR, "error", err)
		}
	}

	// Remove kernel routes. Skipped in port-forward-only mode, which does
	// not manage kernel routes on the provider bridge.
	if !a.cfg.PortForwardOnly {
		currentKernel, err := a.routing.listKernelRoutes()
		if err != nil {
			slog.Error("failed to list kernel routes", "error", err)
		} else {
			for _, e := range currentKernel {
				slog.Info("removing kernel route", "ip", e.IP, "dev", e.Dev, "reason", reason)
				if err := a.routing.DelKernelRoute(e.IP, e.Dev); err != nil {
					slog.Error("failed to remove kernel route", "ip", e.IP, "dev", e.Dev, "error", err)
				}
			}
		}
	}

	if len(delFRR) > 0 {
		if err := a.routing.RefreshBGP(); err != nil {
			slog.Warn("BGP soft-refresh failed, peers may wait for MRAI timer", "error", err)
		}
	}
}

// cleanup removes all managed routes, OVS flows, and OVN NB entries on shutdown.
func (a *Agent) cleanup() {
	a.removeAllRoutes("shutdown cleanup")

	// Clean up FRR prefix-list entries.
	if err := a.routing.ReconcileFRRPrefixList(nil); err != nil {
		slog.Error("failed to cleanup FRR prefix-list", "error", err)
	}

	// OVS flows, the dedicated kernel routing table, and agent-created
	// segment VLAN interfaces are OVN-gateway specific and never created
	// in port-forward-only mode.
	if !a.cfg.PortForwardOnly {
		if err := a.routing.RemoveOVSFlows(); err != nil {
			slog.Error("failed to remove OVS flows", "error", err)
		}
		if err := a.routing.CleanupRoutingTable(); err != nil {
			slog.Error("failed to flush routing table", "error", err)
		}
		if err := a.routing.TeardownSegmentInterfaces(); err != nil {
			slog.Error("failed to remove segment interfaces", "error", err)
		}
	}
	// Tear down port forwarding before veth leak (DNAT return route uses veth).
	if err := a.routing.TeardownPortForward(); err != nil {
		slog.Error("failed to tear down port forwarding", "error", err)
	}
	if err := a.routing.TeardownVethLeak(); err != nil {
		slog.Error("failed to tear down veth VRF leak", "error", err)
	}

	// The bridge IP and managed OVN NB entries only exist in full mode.
	if a.cfg.PortForwardOnly {
		return
	}
	if err := a.routing.RemoveBridgeIP(a.cfg.BridgeIP); err != nil {
		slog.Error("failed to remove bridge IP", "error", err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanupCancel()
	if err := a.ovn.RemoveManagedNBEntries(cleanupCtx); err != nil {
		slog.Error("failed to remove managed OVN NB entries", "error", err)
	}
}

// consecutiveReAddThreshold is the number of consecutive reconcile cycles
// with route re-adds before logging escalates from Warn to Error.
const consecutiveReAddThreshold = 3

// verifyRoutes checks that all desired IPs still have both a kernel route
// (on the right interface) and an FRR static route after route mutations.
// Any route that disappeared (e.g. due to a vtysh race or unexpected FRR
// behaviour) is re-added immediately so that existing connections are not
// disrupted. Every desired IP is agent-produced by construction, so all are
// verified — ownership is no longer inferred from CIDR membership.
//
// Returns the number of routes that had to be re-added (0 means all routes
// were present). The agent tracks consecutive non-zero results and escalates
// logging to help operators detect persistent route instability. IPs in
// skipKernelRoute are not re-added at the kernel level: their VLAN segment did
// not resolve this cycle, so their existing route must be left untouched
// rather than recreated on the bridge fallback.
func (a *Agent) verifyRoutes(desiredIPs []string, ipDev map[string]string, skipKernelRoute map[string]bool) int {
	// Re-read current FRR routes.
	currentFRR, err := a.routing.ListFRRRoutes()
	if err != nil {
		slog.Error("post-change FRR route verification failed", "error", err)
		return 0
	}
	frrSet := make(map[string]bool, len(currentFRR))
	for _, ip := range currentFRR {
		frrSet[ip] = true
	}

	// Re-read current kernel routes. In port-forward-only mode kernel
	// routes are not managed (see ensureRoutes), so this is skipped.
	manageKernel := !a.cfg.PortForwardOnly
	kernelSet := make(map[kernelRouteEntry]bool)
	if manageKernel {
		currentKernel, err := a.routing.listKernelRoutes()
		if err != nil {
			slog.Error("post-change kernel route verification failed", "error", err)
			return 0
		}
		for _, e := range currentKernel {
			kernelSet[e] = true
		}
	}

	var reAddFRR []string
	reAddKernel := 0
	for _, ip := range desiredIPs {
		if !frrSet[ip] {
			slog.Warn("FRR route missing after route change, re-adding", "ip", ip)
			reAddFRR = append(reAddFRR, ip)
		}
		dev := a.desiredRouteDev(ip, ipDev)
		if manageKernel && !skipKernelRoute[ip] && !kernelSet[kernelRouteEntry{IP: ip, Dev: dev}] {
			slog.Warn("kernel route missing after route change, re-adding", "ip", ip, "dev", dev)
			if err := a.routing.AddKernelRoute(ip, dev); err != nil {
				slog.Error("failed to re-add kernel route", "ip", ip, "dev", dev, "error", err)
			}
			reAddKernel++
		}
	}

	if len(reAddFRR) > 0 {
		if err := a.routing.AddFRRRoutes(reAddFRR); err != nil {
			slog.Error("failed to re-add FRR routes", "count", len(reAddFRR), "error", err)
		}
	}

	totalReAdds := len(reAddFRR) + reAddKernel
	recordRouteReAdds(len(reAddFRR), reAddKernel)
	if totalReAdds > 0 {
		a.consecutiveReAdds++
		if a.consecutiveReAdds >= consecutiveReAddThreshold {
			slog.Error("persistent route instability detected: routes required re-adding for multiple consecutive cycles",
				"consecutive_cycles", a.consecutiveReAdds,
				"re_added_this_cycle", totalReAdds,
			)
		}
	} else {
		a.consecutiveReAdds = 0
	}
	setConsecutiveReAdds(a.consecutiveReAdds)

	return totalReAdds
}

// checkFRRRouteActivity surfaces desired routes that exist as FRR static
// routes but are not selected/installed, and therefore not advertised via BGP.
// Such a route is invisible to ensureRoutes — ListFRRRoutes still reports it as
// present — yet leaves the FIP unreachable from outside. Re-adding would not
// help (the route already exists; the next-hop is the fault), so the condition
// is reported loudly and exposed through the inactive_routes metric for
// alerting. A failed check leaves the metric at its previous value rather than
// falsely resetting it to zero.
func (a *Agent) checkFRRRouteActivity(desiredIPs []string) {
	inactive, err := a.routing.InactiveFRRRoutes(desiredIPs)
	if err != nil {
		slog.Warn("could not verify FRR route activity", "error", err)
		return
	}
	setInactiveRoutes(len(inactive))
	if len(inactive) > 0 {
		slog.Error("FRR static routes are configured but inactive — these FIPs are not advertised via BGP",
			"count", len(inactive), "ips", inactive, "vrf", a.cfg.VRFName)
	}
}

// uniqueIPs deduplicates and sorts a list of IP strings.
func uniqueIPs(ips []string) []string {
	seen := make(map[string]bool, len(ips))
	var result []string
	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip != "" && !seen[ip] {
			seen[ip] = true
			result = append(result, ip)
		}
	}
	sort.Strings(result)
	return result
}
