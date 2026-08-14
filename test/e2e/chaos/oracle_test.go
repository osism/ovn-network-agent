package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// oracleFor wires an oracle over a canned lab and per-gateway config docs,
// with a generous settle budget the fakeClock burns through instantly.
func oracleFor(t *testing.T, respond func([]string) (string, error), docs map[string]map[string]any) *oracle {
	t.Helper()
	cmd := &fakeCommander{respond: respond}
	o := newOracle(newTestLab(cmd, newFakeClock()), oracleApplier(docs))
	o.settleTimeout = 90 * time.Second
	return o
}

func hasViolation(v []violationRecord, kind, target string) bool {
	for _, r := range v {
		if r.Kind == kind && r.Target == target {
			return true
		}
	}
	return false
}

func hasTarget(v []violationRecord, target string) bool {
	for _, r := range v {
		if r.Target == target {
			return true
		}
	}
	return false
}

// A lab that already matches its configuration passes the settle on the second
// (confirmation) evaluation, with no violations and a non-negative convergence
// time.
func TestSettlePassesOnAConvergedLab(t *testing.T) {
	fx := newOracleLab(t)
	o := oracleFor(t, fx.respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	v, convergedMS := o.verify(ctx)

	if len(v) != 0 {
		t.Fatalf("a converged lab reported violations: %+v", v)
	}
	if convergedMS < 0 {
		t.Fatalf("convergedMS = %d, want >= 0 on a pass", convergedMS)
	}
}

// A divergence that never heals becomes a violation once the settle window
// expires — naming the gateway and the plane that stayed wrong.
func TestSettleDeadlineConvertsDivergenceIntoViolations(t *testing.T) {
	fx := newOracleLab(t)
	fx.dropKernel["gateway-1"] = "192.0.2.10" // one kernel route missing on every poll
	o := oracleFor(t, fx.respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	v, convergedMS := o.verify(ctx)

	if convergedMS != -1 {
		t.Fatalf("convergedMS = %d, want -1 when the lab never converges", convergedMS)
	}
	found := false
	for _, r := range v {
		if r.Kind == violationExpectedState && r.Target == "gateway-1" && strings.Contains(r.Detail, "kernel") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no kernel expected-state violation naming gateway-1: %+v", v)
	}
}

// The plane #247 left invisible. A gateway whose vrf-provider lost its default
// route drops everything bound for a destination the VRF does not host — the
// reply to a port-forward VIP whose gateway port lives on another chassis,
// above all — while every other plane still matches its expectation. Only the
// probe saw it before; now a sweep does.
func TestSettleFlagsAGatewayWithoutAVRFDefaultRoute(t *testing.T) {
	fx := newOracleLab(t)
	// gateway-3 is a standby here, which is the point: it needs the route to
	// reach the fabric whether or not it currently announces anything.
	fx.noVRFDefault["gateway-3"] = true
	o := oracleFor(t, fx.respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	v, convergedMS := o.verify(ctx)

	if convergedMS != -1 {
		t.Fatalf("convergedMS = %d, want -1 with a gateway missing its VRF default route", convergedMS)
	}
	found := false
	for _, r := range v {
		if r.Kind == violationExpectedState && r.Target == "gateway-3" &&
			strings.Contains(r.Detail, "vrf-default") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a gateway without a VRF default route was not flagged: %+v", v)
	}
}

// The gauge is the agent's own view of the same plane, so a lab where the route
// is there and the agent says so passes — the check must not fire on a healthy
// node just because it now reads one more series.
func TestSettlePassesWithTheVRFDefaultRoutePresent(t *testing.T) {
	fx := newOracleLab(t)
	o := oracleFor(t, fx.respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	v, convergedMS := o.verify(ctx)

	for _, r := range v {
		if strings.Contains(r.Detail, "vrf-default") {
			t.Fatalf("a healthy lab was flagged for the VRF default route: %+v", r)
		}
	}
	if convergedMS < 0 {
		t.Fatalf("convergedMS = %d, want >= 0 on a pass", convergedMS)
	}
}

// A transient divergence that heals within the window is absorbed: the retries
// see it clear and the settle passes.
func TestSettleRetriesAbsorbTransientDivergence(t *testing.T) {
	fx := newOracleLab(t)
	var kernelCalls int
	respond := func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if strings.Contains(line, "clab-ovn-e2e-gateway-1") && strings.Contains(line, "ip -j route show proto 44") {
			kernelCalls++
			if kernelCalls <= 2 { // the first two evaluations miss a route, then it heals
				return `[{"dst":"192.0.2.1","dev":"br-ex"},{"dst":"192.0.2.12","dev":"br-ex"}]`, nil
			}
		}
		return fx.respond(argv)
	}
	o := oracleFor(t, respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	v, convergedMS := o.verify(ctx)

	if len(v) != 0 {
		t.Fatalf("a divergence that healed still failed the settle: %+v", v)
	}
	if convergedMS < 0 {
		t.Fatalf("convergedMS = %d, want >= 0 once the lab heals", convergedMS)
	}
}

// The flap indicators fail the settle two ways: a sustained consecutive-readds
// gauge, and a route-readds counter that keeps climbing between the two
// confirmation evaluations.
func TestSettleFailsOnFlapIndicators(t *testing.T) {
	t.Run("consecutive readds gauge", func(t *testing.T) {
		fx := newOracleLab(t)
		fx.consecutive["gateway-1"] = 2
		o := oracleFor(t, fx.respond, fullModeDocs(t))
		ctx := context.Background()

		if err := o.prime(ctx); err != nil {
			t.Fatalf("prime: %v", err)
		}
		v, convergedMS := o.verify(ctx)

		if convergedMS != -1 {
			t.Fatalf("convergedMS = %d, want -1 on a persistent flap", convergedMS)
		}
		if !hasViolation(v, violationRouteFlap, "gateway-1") {
			t.Fatalf("no route-flap violation for the flapping gateway: %+v", v)
		}
	})

	t.Run("route readds counter climbs across the confirmation", func(t *testing.T) {
		fx := newOracleLab(t)
		var scrapes int
		respond := func(argv []string) (string, error) {
			line := strings.Join(argv, " ")
			if strings.Contains(line, "clab-ovn-e2e-gateway-1") && strings.Contains(line, "/metrics") {
				scrapes++ // green gauges, but the counter never settles
				return climbingMetrics(scrapes), nil
			}
			return fx.respond(argv)
		}
		o := oracleFor(t, respond, fullModeDocs(t))
		ctx := context.Background()

		if err := o.prime(ctx); err != nil {
			t.Fatalf("prime: %v", err)
		}
		v, convergedMS := o.verify(ctx)

		if convergedMS != -1 {
			t.Fatalf("convergedMS = %d, want -1 when the counter never settles", convergedMS)
		}
		found := false
		for _, r := range v {
			if r.Kind == violationRouteFlap && r.Target == "gateway-1" && strings.Contains(r.Detail, "route_readds_total") {
				found = true
			}
		}
		if !found {
			t.Fatalf("no route-flap violation from the climbing counter: %+v", v)
		}
	})
}

// climbingMetrics is a green metrics scrape whose route_readds_total counter is
// n — used to prove the confirmation catches a counter that never settles.
func climbingMetrics(n int) string {
	return fmt.Sprintf("HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\n"+
		"ovn_network_agent_consecutive_readds 0\n"+
		"ovn_network_agent_inactive_routes 0\n"+
		"ovn_network_agent_route_readds_total{plane=\"kernel\"} %d\n"+
		"ovn_network_agent_route_readds_total{plane=\"frr\"} 0\n", n)
}

// A priority-0 Gateway_Chassis row is drain residue — a violation — unless it
// was already 0 at prime, or a drain-enabled termination explains it.
func TestDrainResidueIsAViolationOnlyWithoutADrainEnabledTermination(t *testing.T) {
	fx := newOracleLab(t)
	// A row already parked at priority 0 when the run started is tolerated.
	fx.extraGC = append(fx.extraGC, gcExtra{uuid: "gc-zero", name: "lr0-public-standby", chassis: "gateway-3"})
	docs := fullModeDocs(t)
	docs["gateway-2"]["drain_on_shutdown"] = true // gateway-2's config drains
	o := oracleFor(t, fx.respond, docs)
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// A fresh priority-0 row for gateway-2, with no termination to explain it.
	fx.extraGC = append(fx.extraGC, gcExtra{uuid: "gc-new", name: "lr0-public-extra", chassis: "gateway-2"})
	v, _ := o.verify(ctx)
	if !hasViolation(v, violationDrainDisabled, "gateway-2") {
		t.Fatalf("a new priority-0 row was not flagged as drain residue: %+v", v)
	}
	if hasTarget(v, "gateway-3") {
		t.Fatalf("a row already at priority 0 at prime was flagged: %+v", v)
	}

	// After a drain-enabled termination of gateway-2 (marker present, so the
	// config's drain wins), the same row is tolerated and the settle passes.
	fx.marker["gateway-2"] = true
	if err := o.observeInject(ctx, "agent-terminate", "gateway-2"); err != nil {
		t.Fatalf("observeInject: %v", err)
	}
	v2, convergedMS := o.verify(ctx)
	if len(v2) != 0 {
		t.Fatalf("a drain-enabled termination still flagged the residue: %+v", v2)
	}
	if convergedMS < 0 {
		t.Fatalf("convergedMS = %d, want >= 0 once the residue is explained", convergedMS)
	}
}

// A port-forward-only gateway runs with no OVN view, so a managed NB row that
// appears against it after prime is a write it could not have made.
func TestPFOnlyGatewayMustNotGrowManagedNBRows(t *testing.T) {
	fx := newOracleLab(t)
	docs := fullModeDocs(t)
	docs["gateway-3"] = map[string]any{"bridge_dev": "br-ex"} // no OVN remotes → pf-only
	o := oracleFor(t, fx.respond, docs)
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// A managed static route attributed to the pf-only gateway appears.
	fx.managed = append(fx.managed, managedRoute{uuid: "sr-pf", prefix: "203.0.113.5/32", chassis: "gateway-3"})
	v, convergedMS := o.verify(ctx)

	if convergedMS != -1 {
		t.Fatalf("convergedMS = %d, want -1 when a pf-only gateway grows an NB row", convergedMS)
	}
	if !hasViolation(v, violationOVNTouched, "gateway-3") {
		t.Fatalf("a pf-only gateway growing a managed NB row was not flagged: %+v", v)
	}
}

// The elected owner of a multi-candidate chassisredirect port must strictly
// outrank every peer; a single-candidate group is exempt.
func TestPriorityLeadViolationWhenAPeerOutranksTheOwner(t *testing.T) {
	inverted := map[string]int{"gateway-1": 10, "gateway-2": 30, "gateway-3": 10}

	t.Run("peer outranks the owner", func(t *testing.T) {
		fx := newOracleLab(t)
		fx.priority = inverted
		o := oracleFor(t, fx.respond, fullModeDocs(t))
		ctx := context.Background()

		if err := o.prime(ctx); err != nil {
			t.Fatalf("prime: %v", err)
		}
		v, _ := o.verify(ctx)

		found := false
		for _, r := range v {
			if r.Kind == violationExpectedState && r.Target == "cr-lr0-public" && strings.Contains(r.Detail, "outrank") {
				found = true
			}
		}
		if !found {
			t.Fatalf("an owner outranked by a peer was not flagged: %+v", v)
		}
	})

	t.Run("single-candidate group is exempt", func(t *testing.T) {
		fx := newOracleLab(t)
		fx.priority = inverted
		fx.lrpGC = []string{"gc-gateway-1"} // only the owner is a candidate
		o := oracleFor(t, fx.respond, fullModeDocs(t))
		ctx := context.Background()

		if err := o.prime(ctx); err != nil {
			t.Fatalf("prime: %v", err)
		}
		v, convergedMS := o.verify(ctx)

		if hasTarget(v, "cr-lr0-public") {
			t.Fatalf("a single-candidate group was flagged for priority lead: %+v", v)
		}
		if convergedMS < 0 {
			t.Fatalf("convergedMS = %d, want >= 0 on an otherwise converged lab", convergedMS)
		}
	})
}

// A managed route naming a chassis that has left SB is a violation — the
// stale-chassis cleanup should have reclaimed it.
func TestManagedEntryNamingAVanishedChassisIsAViolation(t *testing.T) {
	fx := newOracleLab(t)
	fx.managed = append(fx.managed, managedRoute{uuid: "sr-gone", prefix: "192.0.2.77/32", chassis: "gateway-gone"})
	o := oracleFor(t, fx.respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	v, convergedMS := o.verify(ctx)

	if convergedMS != -1 {
		t.Fatalf("convergedMS = %d, want -1 with a vanished-chassis route present", convergedMS)
	}
	found := false
	for _, r := range v {
		if r.Kind == violationExpectedState && r.Target == "gateway-gone" && strings.Contains(r.Detail, "SB Chassis") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a managed route naming a vanished chassis was not flagged: %+v", v)
	}
}

// A drain question the oracle cannot ask at inject time tolerates the target's
// priority-0 residue rather than fabricating a drain-while-disabled violation —
// the same "could not ask" split agentAlive and checkError make.
func TestObserveInjectToleratesAnUnanswerableDrainQuestion(t *testing.T) {
	fx := newOracleLab(t)
	// The marker check on gateway-2 is not exit 1 (marker absent) but a
	// question that could not be asked at all — a docker daemon under load.
	respond := func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if strings.Contains(line, "clab-ovn-e2e-gateway-2") && strings.Contains(line, "test -f "+profileMarkerPath) {
			return "", errBoom
		}
		return fx.respond(argv)
	}
	o := oracleFor(t, respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// A fresh priority-0 row appears for gateway-2 — drain residue on its face.
	fx.extraGC = append(fx.extraGC, gcExtra{uuid: "gc-new", name: "lr0-public-extra", chassis: "gateway-2"})

	err := o.observeInject(ctx, "agent-terminate", "gateway-2")
	if err == nil {
		t.Fatal("observeInject swallowed the unanswerable drain question instead of surfacing it")
	}
	v, convergedMS := o.verify(ctx)
	if hasViolation(v, violationDrainDisabled, "gateway-2") {
		t.Fatalf("an unanswerable drain question fabricated a drain-while-disabled violation: %+v", v)
	}
	if convergedMS < 0 {
		t.Fatalf("convergedMS = %d, want >= 0: the tolerated residue leaves the lab converged", convergedMS)
	}
}

// A drain tolerance covers the window that follows the fault that earned it,
// not the rest of the run: the agent restores a drained row to priority 1 on
// startup, so once a window has consumed the tolerance the same row still at 0
// is residue nothing explains.
func TestDrainToleranceExpiresWithTheSettleWindowThatConsumesIt(t *testing.T) {
	fx := newOracleLab(t)
	docs := fullModeDocs(t)
	docs["gateway-2"]["drain_on_shutdown"] = true
	o := oracleFor(t, fx.respond, docs)
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// A drain-enabled termination of gateway-2, and the priority-0 row it
	// legitimately leaves behind.
	fx.marker["gateway-2"] = true
	if err := o.observeInject(ctx, "agent-terminate", "gateway-2"); err != nil {
		t.Fatalf("observeInject: %v", err)
	}
	fx.extraGC = append(fx.extraGC, gcExtra{uuid: "gc-new", name: "lr0-public-extra", chassis: "gateway-2"})

	if v, _ := o.verify(ctx); hasViolation(v, violationDrainDisabled, "gateway-2") {
		t.Fatalf("the window following the drain did not tolerate its residue: %+v", v)
	}

	// A later window, with no new termination to explain it: the same row is
	// now a chassis that gave up its mastership without being asked to.
	v, convergedMS := o.verify(ctx)
	if !hasViolation(v, violationDrainDisabled, "gateway-2") {
		t.Fatalf("a consumed drain tolerance still exempted gateway-2: %+v", v)
	}
	if convergedMS != -1 {
		t.Fatalf("convergedMS = %d, want -1 with unexplained drain residue present", convergedMS)
	}
}

// A prefix-list expected gone but left behind holding no entries is a partial
// cleanup, not a pass: the standby emptied the list without removing the object.
func TestAnEmptyButPresentPrefixListOnAStandbyIsAViolation(t *testing.T) {
	fx := newOracleLab(t)
	// gateway-2 is a standby (gateway-1 owns cr-lr0-public), so the agent is
	// expected to have cleaned the list away entirely. Instead the entries are
	// gone but the list object survives.
	respond := func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if strings.Contains(line, "clab-ovn-e2e-gateway-2") &&
			strings.Contains(line, "show ip prefix-list "+announcedPrefixList) {
			return "ip prefix-list " + announcedPrefixList + ": 0 entries\n", nil
		}
		return fx.respond(argv)
	}
	o := oracleFor(t, respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	v, convergedMS := o.verify(ctx)

	if convergedMS != -1 {
		t.Fatalf("convergedMS = %d, want -1 with a stale prefix-list object present", convergedMS)
	}
	found := false
	for _, r := range v {
		if r.Kind == violationExpectedState && r.Target == "gateway-2" &&
			strings.Contains(r.Detail, "prefix-list") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a standby leaking an emptied prefix-list object was not flagged: %+v", v)
	}
}

// The absence checks that read a plane as empty key off the tool's own exit 1.
// Every other non-zero exit is the docker CLI failing — a stopped container, an
// unreachable daemon, an expired cmdTimeout — and reading it as "absent" would
// report a plane converged that was never read at all. The baked config carries
// no port_forwards, so the DNAT, masquerade and VIP expectations are empty and
// a swallowed failure diffs empty against empty and passes.
func TestAnUnreadableDataPlaneIsNotReadAsAbsent(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
	}{
		{name: "nftables ruleset", cmd: "nft list table ip " + agentNftTable},
		{name: "managed VIP addresses", cmd: "ip -j addr show dev loopback1"},
		// This one would fail either way, but for the wrong reason: a
		// swallowed read leaves the plane looking absent, and the oracle
		// would report a missing default route on a gateway whose routing
		// table it never managed to read.
		{name: "VRF default route", cmd: "ip -j route show vrf " + vrfProvider + " default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newOracleLab(t)
			// Exit 125 is docker's own "container is not running" — the command
			// inside never ran, so it answered nothing.
			respond := func(argv []string) (string, error) {
				line := strings.Join(argv, " ")
				if strings.Contains(line, "clab-ovn-e2e-gateway-1") && strings.Contains(line, tc.cmd) {
					return "", errExit(t, 125)
				}
				return fx.respond(argv)
			}
			o := oracleFor(t, respond, fullModeDocs(t))
			ctx := context.Background()

			if err := o.prime(ctx); err != nil {
				t.Fatalf("prime: %v", err)
			}
			v, convergedMS := o.verify(ctx)

			if convergedMS != -1 {
				t.Fatalf("convergedMS = %d, want -1: a plane that could not be read is not a converged plane", convergedMS)
			}
			if !hasViolation(v, violationOracleSetup, "gateway-1") {
				t.Fatalf("a plane the oracle could not read was reported converged: %+v", v)
			}
		})
	}
}

// A query the oracle cannot answer fails the settle loudly: the deadline turns
// the unanswerable poll into a violation carrying the underlying error.
func TestAnUnanswerableOracleQueryFailsTheSettleLoudly(t *testing.T) {
	fx := newOracleLab(t)
	o := oracleFor(t, fx.respond, fullModeDocs(t))
	ctx := context.Background()

	if err := o.prime(ctx); err != nil {
		t.Fatalf("prime: %v", err)
	}
	fx.snapErr = true // the OVN snapshot fails on every poll from here on

	v, convergedMS := o.verify(ctx)

	if convergedMS != -1 {
		t.Fatalf("convergedMS = %d, want -1 when the oracle cannot observe", convergedMS)
	}
	found := false
	for _, r := range v {
		if r.Kind == violationOracleSetup && strings.Contains(r.Detail, "boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an unanswerable query did not fail loudly with the error: %+v", v)
	}
}
