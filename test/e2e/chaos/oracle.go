package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// =============================================================================
// Config-aware settle oracle
// =============================================================================
//
// The oracle closes the loop the baseline sweep (checks.go) and the recovery
// gate (engine.go) leave open: those assert the lab is reachable, this asserts
// it converged to the exact state its configuration demands. Between faults the
// engine hands it a settle window; the oracle polls until every gateway's live
// data plane matches computeExpectation (expect.go) or the window expires.
//
// It verifies eight per-gateway planes (kernel routes with their device, FRR
// statics, the vrf-provider default route, the ANNOUNCED-NETWORKS prefix-list,
// hairpin flows, MAC-tweak flow count, nftables DNAT/masquerade, managed VIP
// addresses) and the upstream announcements, plus four snapshot-wide
// invariants (chassisredirect priority lead, drain residue, vanished-chassis
// hygiene, a frozen NB under port-forward-only) and a metrics gate. Everything
// it reads comes through observe.go; it draws nothing from the engine's rng and
// never touches the run's decision stream.

const (
	// settlePollInterval is how often the settle loop re-observes the lab.
	settlePollInterval = 5 * time.Second

	// settleConfirmDelay is the gap between the first all-green evaluation and
	// its confirmation. It must exceed the slow reconcile cadence
	// (slowCadence = "15s" in flips.go) so one confirmation gap always spans at
	// least one reconcile: a lab that only looks converged because the agent
	// has not reconciled yet is caught when the next reconcile churns a plane.
	settleConfirmDelay = 20 * time.Second
)

// oracle verifies one settle window against the config-aware expected state.
type oracle struct {
	lab *lab
	ap  *applier

	settleTimeout time.Duration

	tolerated0      map[string]bool   // Gateway_Chassis row UUIDs already at priority 0 at prime
	drainedLegit    map[string]bool   // gateways whose last disruptive action ran with drain effectively enabled; consumed by the next settle window
	baselineManaged map[string]string // managed static-route row UUID → rendered content at prime
	drainEnv        map[string]string // per-gateway OVN_NETWORK_DRAIN_ON_SHUTDOWN container env ("" = unset)
}

func newOracle(l *lab, ap *applier) *oracle {
	return &oracle{
		lab:             l,
		ap:              ap,
		tolerated0:      map[string]bool{},
		drainedLegit:    map[string]bool{},
		baselineManaged: map[string]string{},
		drainEnv:        map[string]string{},
	}
}

// prime records the baseline the settle loop reasons against, run once after
// the start state is green: the Gateway_Chassis rows legitimately already at
// priority 0, the managed static routes as the port-forward-only frozen-NB
// check will compare against, and each gateway's drain environment.
func (o *oracle) prime(ctx context.Context) error {
	snap, err := snapshotOVN(ctx, o.lab)
	if err != nil {
		return fmt.Errorf("prime oracle: %w", err)
	}
	for uuid, row := range snap.GatewayChassis {
		if row.Priority == 0 {
			o.tolerated0[uuid] = true
		}
	}
	for _, r := range snap.StaticRoutes {
		o.baselineManaged[r.UUID] = renderManagedRoute(r)
	}
	for _, gw := range gatewayNames() {
		env, err := o.drainEnvOf(ctx, gw)
		if err != nil {
			return fmt.Errorf("prime oracle: read drain env of %s: %w", gw, err)
		}
		o.drainEnv[gw] = env
	}
	return nil
}

// drainEnvOf reads a gateway's OVN_NETWORK_DRAIN_ON_SHUTDOWN container
// environment. printenv exits 1 when the variable is unset, which is the
// answer "" — the same exit-1-is-an-answer split agentAlive makes; any other
// error means the question could not be asked.
func (o *oracle) drainEnvOf(ctx context.Context, gw string) (string, error) {
	out, err := o.lab.exec(ctx, gw, "printenv", "OVN_NETWORK_DRAIN_ON_SHUTDOWN")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// observeInject records, before a disruptive action injects, whether the
// target could legitimately end at priority 0 — i.e. its drain is effectively
// enabled. Only the actions that take a chassis down carry that meaning; the
// rest leave the drain classification untouched.
func (o *oracle) observeInject(ctx context.Context, action, target string) error {
	switch action {
	case "agent-terminate", "gateway-restart", "config-flip", "double-failover":
	default:
		return nil
	}

	drain, err := o.effectiveDrain(ctx, target)
	if err != nil {
		// The drain question could not be asked (docker under load, the
		// container gone mid-inject). An unanswerable question tolerates any
		// priority-0 residue the target leaves rather than inventing a
		// drain-while-disabled violation — the same "could not ask" split
		// agentAlive and checkError make.
		o.drainedLegit[target] = true
		return err
	}
	o.drainedLegit[target] = drain

	// A double failover SIGTERMs the target (which drains if enabled) and
	// SIGKILLs the ring-next peer (control.go injectDoubleFailover). A SIGKILL
	// never drains, so the peer can never legitimately end at priority 0 —
	// mark it false, resetting any drain a previous action left recorded.
	if action == "double-failover" {
		o.drainedLegit[nextGateway(target)] = false
	}
	return nil
}

// effectiveDrain resolves the drain the target actually runs with. When the
// profile marker is present the entrypoint unsets the deploy-time env override
// (gwnode-entrypoint.sh yield_config_to_chaos_profile), so the config doc
// wins; otherwise the env layer beats the file, and only an explicit "true"
// drains.
func (o *oracle) effectiveDrain(ctx context.Context, gw string) (bool, error) {
	marker, err := o.markerPresent(ctx, gw)
	if err != nil {
		return false, err
	}
	if marker {
		return docDrainOnShutdown(o.ap.current[gw]), nil
	}
	if env := o.drainEnv[gw]; env != "" {
		return env == "true", nil
	}
	return docDrainOnShutdown(o.ap.current[gw]), nil
}

// markerPresent reports whether the chaos-profile marker file is on the
// gateway. `test -f` exits 1 when it is absent, which is the answer.
func (o *oracle) markerPresent(ctx context.Context, gw string) (bool, error) {
	if _, err := o.lab.exec(ctx, gw, "test", "-f", profileMarkerPath); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// settleFailure is one broken expectation in a single evaluation. The engine
// stamps the tick and journal offset onto the violations verify returns.
type settleFailure struct {
	kind   string
	target string
	detail string
}

// evaluation is the verdict of one poll: the failures it found, and each
// gateway's summed route_readds_total for the confirmation delta check.
type evaluation struct {
	failures []settleFailure
	readds   map[string]int
}

// verify polls the lab against its config-aware expected state until two
// consecutive evaluations are all-green (a pass) or settleTimeout expires (the
// last evaluation's failures become violations). It is clocked by the lab's
// now/sleep, so a run replays without wall-clock time.
//
// On pass it returns (nil, convergedMS) where convergedMS is the wall-clock
// from the loop start to the first of the two green evaluations. On deadline it
// returns the failures as violations and convergedMS = -1 — the lab never
// converged, so there is no convergence time to report.
//
// The window consumes the drain tolerances the faults before it recorded: a
// tolerance excuses the priority-0 residue of the fault that earned it, and
// this is the window that fault's residue shows up in. Carrying it further
// would exempt the gateway for the rest of the run — the agent restores a
// drained row to priority 1 on startup (RestoreDrainedGateways), so once a
// window has seen the target restored, a later priority-0 row is residue no
// drain explains and is exactly what violationDrainDisabled exists to catch.
func (o *oracle) verify(ctx context.Context) ([]violationRecord, int64) {
	defer clear(o.drainedLegit)

	start := o.lab.now()
	deadline := start.Add(o.settleTimeout)
	var last []settleFailure

	for o.lab.now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}

		first := o.evaluate(ctx)
		if len(first.failures) > 0 {
			last = first.failures
			o.lab.sleep(settlePollInterval)
			continue
		}

		// First all-green evaluation: record convergence and confirm it holds
		// across a reconcile gap, with route_readds_total steady between the
		// two — a delta means the plane is still churning under the surface.
		convergedMS := o.lab.now().Sub(start).Milliseconds()
		o.lab.sleep(settleConfirmDelay)
		second := o.evaluate(ctx)
		flaps := readdFlaps(first.readds, second.readds)
		switch {
		case len(second.failures) > 0:
			last = second.failures
		case len(flaps) > 0:
			last = flaps
		default:
			return nil, convergedMS
		}
		o.lab.sleep(settlePollInterval)
	}

	return toViolations(last), -1
}

// evaluate takes one snapshot and checks every plane and invariant against it.
// A query it cannot answer is a single oracle-setup failure for that poll, not
// an aborted loop: the deadline turns the last poll's failures into violations,
// mirroring checks.go's checkError philosophy.
func (o *oracle) evaluate(ctx context.Context) evaluation {
	snap, err := snapshotOVN(ctx, o.lab)
	if err != nil {
		return evaluation{failures: []settleFailure{{kind: violationOracleSetup, detail: err.Error()}}}
	}
	upstream, err := observeUpstream(ctx, o.lab)
	if err != nil {
		return evaluation{failures: []settleFailure{{kind: violationOracleSetup, detail: err.Error()}}}
	}

	ev := evaluation{readds: map[string]int{}}
	for _, gw := range gatewayNames() {
		exp := computeExpectation(snap, gw, o.ap.current[gw], o.ap.mgmtIP[gw])
		obs, err := observeGateway(ctx, o.lab, gw, exp.PortForwardDev)
		if err != nil {
			ev.failures = append(ev.failures, settleFailure{kind: violationOracleSetup, target: gw, detail: err.Error()})
			continue
		}
		ev.readds[gw] = obs.metrics.routeReAddsTotal
		ev.failures = append(ev.failures, comparePlanes(gw, exp, obs, upstream)...)
		ev.failures = append(ev.failures, metricsGate(gw, obs)...)
	}
	ev.failures = append(ev.failures, o.priorityLeadFailures(snap)...)
	ev.failures = append(ev.failures, o.drainResidueFailures(snap)...)
	ev.failures = append(ev.failures, o.vanishedChassisFailures(snap)...)
	ev.failures = append(ev.failures, o.pfOnlyFrozenFailures(snap)...)
	return ev
}

// comparePlanes diffs each per-gateway data plane against its expectation,
// honouring the expectation's skip flags. Every mismatch is an expected-state
// failure whose detail names the plane and the sorted missing/unexpected sets.
func comparePlanes(gw string, exp gatewayExpectation, obs observedGateway, upstream upstreamRoutes) []settleFailure {
	var fs []settleFailure
	add := func(plane string, missing, unexpected []string) {
		if len(missing) == 0 && len(unexpected) == 0 {
			return
		}
		fs = append(fs, settleFailure{
			kind:   violationExpectedState,
			target: gw,
			detail: fmt.Sprintf("%s: missing %v, unexpected %v", plane, missing, unexpected),
		})
	}

	if !exp.SkipKernel {
		m, u := diffSet(kernelStrings(exp.KernelRouteDev), setOf(kernelStrings(obs.kernelRoutes)))
		add("kernel", m, u)
	}

	m, u := diffSet(exp.FRRStatic, obs.frrStatic)
	add("frr", m, u)

	// #258 — the leak plane must claim exactly the owned networks: an
	// unexpected entry here is the route that hijacks another chassis's
	// traffic into the local OVN.
	m, u = diffSet(exp.LeakRoutes, obs.leakRoutes)
	add("veth-leak", m, u)

	if !exp.SkipPrefixList {
		if exp.PrefixList == nil {
			// A list expected gone but still present is the failure, whether or
			// not it still holds permit entries — a standby that emptied the
			// list without removing it left the object behind. add() would drop
			// that case: with no entries both its sets are empty and it records
			// nothing.
			if !obs.prefixListAbsent {
				fs = append(fs, settleFailure{
					kind:   violationExpectedState,
					target: gw,
					detail: fmt.Sprintf("prefix-list: want %s absent, have %v",
						announcedPrefixList, keysOf(obs.prefixList)),
				})
			}
		} else {
			m, u := diffSet(exp.PrefixList, obs.prefixList)
			add("prefix-list", m, u)
		}
	}

	if !exp.SkipHairpin {
		m, u := diffSet(exp.Hairpin, obs.hairpin)
		add("hairpin", m, u)
	}

	if exp.MACTweakFlows != -1 && obs.macTweakFlows != exp.MACTweakFlows {
		fs = append(fs, settleFailure{
			kind:   violationExpectedState,
			target: gw,
			detail: fmt.Sprintf("mac-tweak: want %d flows, have %d", exp.MACTweakFlows, obs.macTweakFlows),
		})
	}

	// A bool plane cannot go through add(): with nothing to list as missing or
	// unexpected, both its sets are empty and it records nothing.
	if exp.VRFDefaultRoute && !obs.vrfDefaultRoute {
		fs = append(fs, settleFailure{
			kind:   violationExpectedState,
			target: gw,
			detail: fmt.Sprintf("vrf-default: want a default route in %s, have none", vrfProvider),
		})
	}

	m, u = diffSet(dnatStrings(exp.DNAT), setOf(observedDNATStrings(obs.dnat)))
	add("dnat", m, u)

	m, u = diffSet(exp.HairpinMasquerade, obs.masquerade)
	add("hairpin-masquerade", m, u)

	m, u = diffSet(exp.ManagedVIPs, obs.vips)
	add("managed-vip", m, u)

	// Announcements are a presence-and-staleness bound, not set equality:
	// MustAnnounce ⊆ announced ⊆ AnnounceBound.
	announced := upstream.announcedBy(gw)
	var missing, extra []string
	for _, ip := range exp.MustAnnounce {
		if !announced[ip] {
			missing = append(missing, ip)
		}
	}
	bound := setOf(exp.AnnounceBound)
	for ip := range announced {
		if !bound[ip] {
			extra = append(extra, ip)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	add("announced", missing, extra)

	return fs
}

// metricsGate holds each gateway to a settled reconcile: no consecutive
// re-adds and no inactive routes. A sustained non-zero here is route
// instability the set checks cannot see.
//
// It also holds the agent's own view of the VRF default route against the
// plane comparePlanes reads from the kernel. The two are deliberately
// redundant: the plane check catches the route going away, and this catches
// the agent failing to notice — a detector that silently reports healthy is
// the failure mode #247 was invisible behind in the first place.
func metricsGate(gw string, obs observedGateway) []settleFailure {
	var fs []settleFailure

	if obs.metrics.consecutiveReAdds != 0 || obs.metrics.inactiveRoutes != 0 {
		fs = append(fs, settleFailure{
			kind:   violationRouteFlap,
			target: gw,
			detail: fmt.Sprintf("consecutive_readds=%d inactive_routes=%d",
				obs.metrics.consecutiveReAdds, obs.metrics.inactiveRoutes),
		})
	}

	// Only when the series was actually scraped. Its absence means an agent
	// that does not export it, which is a different problem from an agent
	// reporting the route gone, and reading the zero value as the latter would
	// invent a failure.
	if obs.metrics.vrfDefaultGaugeScraped && obs.metrics.vrfDefaultGauge == 0 {
		fs = append(fs, settleFailure{
			kind:   violationExpectedState,
			target: gw,
			detail: "vrf-default: the agent reports vrf_default_route_present=0",
		})
	}

	return fs
}

// priorityLeadFailures asserts the elected owner of every bound chassisredirect
// port with at least two Gateway_Chassis candidates strictly outranks every
// peer — the invariant HA re-election exists to keep. Single-candidate groups
// (a pinned router) are exempt.
func (o *oracle) priorityLeadFailures(snap ovnSnapshot) []settleFailure {
	var fs []settleFailure
	for _, port := range sortedKeys(snap.CRPortChassis) {
		owner := snap.CRPortChassis[port]
		if owner == "" {
			continue // unbound: a re-election is in flight
		}
		gcs := snap.LRPs[lrpOfCRPort(port)].GatewayChassis
		if len(gcs) < 2 {
			continue
		}
		ownerPrio, ok := ownerPriority(snap, gcs, owner)
		if !ok {
			fs = append(fs, settleFailure{
				kind:   violationExpectedState,
				target: port,
				detail: fmt.Sprintf("%s is owned by %s, which holds no Gateway_Chassis row", port, owner),
			})
			continue
		}
		for _, u := range gcs {
			row, ok := snap.GatewayChassis[u]
			if !ok || row.ChassisName == owner {
				continue
			}
			if row.Priority >= ownerPrio {
				fs = append(fs, settleFailure{
					kind:   violationExpectedState,
					target: port,
					detail: fmt.Sprintf("%s owner %s at priority %d does not outrank %s at priority %d",
						port, owner, ownerPrio, row.ChassisName, row.Priority),
				})
			}
		}
	}
	return fs
}

func ownerPriority(snap ovnSnapshot, gcs []string, owner string) (int, bool) {
	for _, u := range gcs {
		if row, ok := snap.GatewayChassis[u]; ok && row.ChassisName == owner {
			return row.Priority, true
		}
	}
	return 0, false
}

// drainResidueFailures flags a Gateway_Chassis row left at priority 0 that no
// legitimate drain explains: not one already at 0 when the run started
// (tolerated0), and not owned by a gateway whose last disruptive action
// drained it (drainedLegit). A stray priority-0 row is a chassis that gave up
// its mastership without being asked to.
func (o *oracle) drainResidueFailures(snap ovnSnapshot) []settleFailure {
	var fs []settleFailure
	for _, uuid := range sortedKeys(snap.GatewayChassis) {
		row := snap.GatewayChassis[uuid]
		if row.Priority != 0 || o.tolerated0[uuid] || o.drainedLegit[row.ChassisName] {
			continue
		}
		fs = append(fs, settleFailure{
			kind:   violationDrainDisabled,
			target: row.ChassisName,
			detail: fmt.Sprintf("Gateway_Chassis row %s for %s sits at priority 0 without a drain-enabled termination",
				row.Name, row.ChassisName),
		})
	}
	return fs
}

// vanishedChassisFailures asserts every managed static route names a chassis
// still present in SB. A route pointing at a chassis that is gone is one the
// stale-chassis cleanup should have reclaimed.
func (o *oracle) vanishedChassisFailures(snap ovnSnapshot) []settleFailure {
	var fs []settleFailure
	for _, r := range snap.StaticRoutes {
		chassis := r.ExternalIDs[routeChassisKey]
		if chassis == "" || snap.Chassis[chassis] {
			continue
		}
		fs = append(fs, settleFailure{
			kind:   violationExpectedState,
			target: chassis,
			detail: fmt.Sprintf("managed route %s names chassis %s, absent from the SB Chassis table",
				r.IPPrefix, chassis),
		})
	}
	return fs
}

// pfOnlyFrozenFailures asserts a port-forward-only gateway never touches NB: it
// runs with no OVN view, so any managed row it appears on — new, content-
// changed, or carrying a takeover marker — relative to prime is a write it
// could not legitimately have made.
func (o *oracle) pfOnlyFrozenFailures(snap ovnSnapshot) []settleFailure {
	var fs []settleFailure
	for _, gw := range gatewayNames() {
		doc := o.ap.current[gw]
		if docRemoteSet(doc, "ovn_sb_remote") || docRemoteSet(doc, "ovn_nb_remote") {
			continue // not port-forward-only
		}
		for _, r := range snap.StaticRoutes {
			if r.ExternalIDs[routeChassisKey] != gw && r.ExternalIDs[routeAdvertisedKey] != gw {
				continue // not attributed to this pf-only gateway
			}
			baseline, existed := o.baselineManaged[r.UUID]
			switch {
			case !existed:
				fs = append(fs, settleFailure{
					kind:   violationOVNTouched,
					target: gw,
					detail: fmt.Sprintf("pf-only %s grew managed route %s, absent at prime", gw, r.IPPrefix),
				})
			case baseline != renderManagedRoute(r):
				fs = append(fs, settleFailure{
					kind:   violationOVNTouched,
					target: gw,
					detail: fmt.Sprintf("pf-only %s rewrote managed route %s", gw, r.IPPrefix),
				})
			}
		}
	}
	return fs
}

// readdFlaps reports the gateways whose summed route_readds_total moved between
// the two confirmation evaluations — a route still being re-added even though
// every set looks converged.
func readdFlaps(before, after map[string]int) []settleFailure {
	var fs []settleFailure
	for _, gw := range gatewayNames() {
		if after[gw] != before[gw] {
			fs = append(fs, settleFailure{
				kind:   violationRouteFlap,
				target: gw,
				detail: fmt.Sprintf("route_readds_total moved from %d to %d across the settle confirmation",
					before[gw], after[gw]),
			})
		}
	}
	return fs
}

// toViolations converts a poll's failures into the run record's violations,
// sorted for a deterministic record. The tick and journal offset are left for
// the engine to stamp.
func toViolations(failures []settleFailure) []violationRecord {
	if len(failures) == 0 {
		return nil
	}
	sort.Slice(failures, func(i, j int) bool {
		a, b := failures[i], failures[j]
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		if a.target != b.target {
			return a.target < b.target
		}
		return a.detail < b.detail
	})
	out := make([]violationRecord, 0, len(failures))
	for _, f := range failures {
		out = append(out, violationRecord{Kind: f.kind, Target: f.target, Detail: f.detail})
	}
	return out
}

// renderManagedRoute renders a managed static route to a stable string —
// prefix plus its external_ids in sorted-key order — so the port-forward-only
// frozen-NB check can compare a live row against its prime baseline.
func renderManagedRoute(r staticRouteRow) string {
	keys := make([]string, 0, len(r.ExternalIDs))
	for k := range r.ExternalIDs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+r.ExternalIDs[k])
	}
	return r.IPPrefix + "|" + strings.Join(parts, ",")
}

// =============================================================================
// Set helpers
// =============================================================================

// kernelStrings renders an IP→device map as sorted "ip dev <device>" tokens,
// so the kernel plane is compared on the route and its device together.
func kernelStrings(byIP map[string]string) []string {
	out := make([]string, 0, len(byIP))
	for ip, dev := range byIP {
		out = append(out, fmt.Sprintf("%s dev %s", ip, dev))
	}
	sort.Strings(out)
	return out
}

// dnatStrings / observedDNATStrings render DNAT rules to a canonical token so
// the expected and observed rulesets compare as sets.
func dnatStrings(rules []dnatExpectation) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, dnatToken(r.VIP, r.Proto, r.Port, r.Backend, r.DestPort))
	}
	return out
}

func observedDNATStrings(rules []observedDNAT) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, dnatToken(r.vip, r.proto, r.port, r.backend, r.destPort))
	}
	return out
}

func dnatToken(vip, proto string, port int, backend string, destPort int) string {
	return fmt.Sprintf("%s %s dport %d -> %s:%d", vip, proto, port, backend, destPort)
}

// diffSet returns the expected values missing from observed and the observed
// values not expected, both sorted, for a deterministic failure detail.
func diffSet(expected []string, observed map[string]bool) (missing, unexpected []string) {
	exp := setOf(expected)
	for _, e := range expected {
		if !observed[e] {
			missing = append(missing, e)
		}
	}
	for o := range observed {
		if !exp[o] {
			unexpected = append(unexpected, o)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)
	return missing, unexpected
}

func setOf(values []string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
