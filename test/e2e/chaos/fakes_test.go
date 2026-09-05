package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// errBoom is the canned failure the tests inject at the commander seam.
// It stands for the failures that are not the command's own verdict — a
// docker daemon under load, an expired cmdTimeout, a container that is
// gone.
var errBoom = errors.New("boom")

// errExit is what the real commander hands back when the command *inside*
// the container exits non-zero: an *exec.ExitError carrying the code,
// wrapped the way execCommander wraps it. pgrep's exit 1 ("nothing
// matched") is an answer; errBoom is a question that could not be asked.
func errExit(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	if err == nil {
		t.Fatalf("sh -c 'exit %d' reported success", code)
	}
	return fmt.Errorf("docker exec: %w: ", err)
}

// fakeCommander stands in for the docker / containerlab CLIs. It is the
// only seam the tests replace: the engine, the actions and the state
// layering all run for real against it, so what the tests assert on is
// the argv the runner would have executed.
type fakeCommander struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(argv []string) (string, error)
}

func (f *fakeCommander) run(_ context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return "", nil
	}
	return respond(argv)
}

// lines renders every recorded call as a single string, in order.
func (f *fakeCommander) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, argv := range f.calls {
		out = append(out, strings.Join(argv, " "))
	}
	return out
}

// indexOf returns the position of the first recorded call containing
// substr, or -1. Ordering assertions ("the restart policy is disabled
// before the kill") compare two of these.
func (f *fakeCommander) indexOf(substr string) int {
	for i, line := range f.lines() {
		if strings.Contains(line, substr) {
			return i
		}
	}
	return -1
}

func (f *fakeCommander) called(substr string) bool { return f.indexOf(substr) >= 0 }

// count is how many recorded calls contain substr — "the second call
// issued nothing new" is an assertion on this.
func (f *fakeCommander) count(substr string) int {
	n := 0
	for _, line := range f.lines() {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// healthyLabResponses answers every query the runner makes about a lab
// that is behaving: daemons up, chassis registered, cr-lr0-public bound.
// The one thing it models as gone is eth1 (the `ip link show eth1` probe) —
// a container lifecycle event destroys the containerlab veth, which is the
// whole reason the restore path exists. The restore's own verification reads
// eth1 back with a *different* probe (`ip -o -4 addr show eth1`) after the
// rewire has re-created it, so the two answers stand for two points in the
// restore rather than contradicting each other. The container's identity
// (.State.StartedAt) is stable, so no reincarnation is detected unless a test
// varies it deliberately (#217).
func healthyLabResponses(argv []string) (string, error) {
	line := strings.Join(argv, " ")
	switch {
	case strings.Contains(line, "{{.State.Health.Status}}"):
		return "healthy\n", nil
	case strings.Contains(line, "{{.State.Running}}"):
		return "false\n", nil // a killed container stays down until started
	case strings.Contains(line, "{{.State.StartedAt}}"):
		return "2026-07-20T12:00:00.000000000Z\n", nil
	case strings.Contains(line, "ip link show eth1"):
		return "Device \"eth1\" does not exist.", errBoom
	case strings.Contains(line, "ip -o -4 addr show eth1"):
		// The rewire re-created eth1 and gave it the underlay address; the
		// verification reads it back on the incarnation the rewire ran against.
		if link, ok := linkFor(gatewayOf(line)); ok {
			return fmt.Sprintf("2: eth1    inet %s scope global eth1\n", link.gatewayCIDR), nil
		}
		return "", nil
	case strings.Contains(line, "show running-config"):
		// configureGatewayBGP has replaced the entrypoint placeholder with the
		// real session; the verification greps this for the router-id and peer.
		if link, ok := linkFor(gatewayOf(line)); ok {
			return fmt.Sprintf("router bgp 65000 vrf vrf-provider\n bgp router-id %s\n"+
				" neighbor %s remote-as 65001\n", addrOf(link.gatewayCIDR), addrOf(link.upstreamCIDR)), nil
		}
		return "", nil
	case strings.Contains(line, "pgrep -f /usr/local/bin/ovn-network-agent"):
		return "1\n", nil
	case strings.Contains(line, "pgrep -f") && strings.Contains(line, bgpdMatchPattern):
		return "1\n", nil // a healthy upstream runs bgpd
	case strings.Contains(line, "find Chassis name="):
		return strings.TrimPrefix(argv[len(argv)-1], "name=") + "\n", nil
	case strings.Contains(line, "--columns=chassis find Port_Binding"):
		return "0e3d-master-uuid\n", nil
	case strings.Contains(line, "--columns=name list Chassis"):
		return "gateway-1\n", nil
	}
	return "", nil
}

// fakeClock advances only when the code under test sleeps, so a
// ten-minute run takes microseconds and the tick loop stays
// deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) sleep(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// wait is the engine's ctx-aware wait on fake time: a cancelled run does
// not idle out the rest of its tick interval or its fault hold.
func (c *fakeClock) wait(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	c.sleep(d)
	return ctx.Err() == nil
}

// greenProbes is a probeSource whose targets are always up.
type greenProbes struct{}

func (greenProbes) allGreen() bool       { return true }
func (greenProbes) redTargets() []string { return nil }
func (greenProbes) recoverySince(time.Time) map[string]int64 {
	return map[string]int64{"fip-vm1": 0}
}

// redProbes is a probeSource whose data path never comes back: the node
// is up, but nothing behind it answers.
type redProbes struct{}

func (redProbes) allGreen() bool       { return false }
func (redProbes) redTargets() []string { return []string{"fip-vm1"} }
func (redProbes) recoverySince(time.Time) map[string]int64 {
	return map[string]int64{"fip-vm1": 0}
}

// testProfile resolves a profile the tests drive the runner with. Most of
// them want the default one — every layer up, every gateway on the baked
// config — which is what the runner did before profiles existed.
func testProfile(t *testing.T, name string) *profile {
	t.Helper()
	p, err := profileByName(name)
	if err != nil {
		t.Fatalf("profileByName(%q): %v", name, err)
	}
	return p
}

func defaultTestProfile(t *testing.T) *profile { return testProfile(t, defaultProfileName) }

// newTestLab wires a lab against the fake commander and the fake clock.
func newTestLab(cmd commander, clock *fakeClock) *lab {
	return &lab{name: "ovn-e2e", cmd: cmd, sleep: clock.sleep, now: clock.now}
}

// noopActions builds a registry whose faults do nothing but record that
// they ran — the engine's decisions are what the engine tests are about.
func noopActions(names ...string) []*action {
	actions := make([]*action, 0, len(names))
	for _, name := range names {
		actions = append(actions, &action{
			name:           name,
			weight:         1,
			holdMin:        5 * time.Second,
			holdMax:        5 * time.Second,
			recoveryBudget: 60 * time.Second,
			inject:         func(context.Context, *lab, string, int) error { return nil },
			restore:        func(context.Context, *lab, string) error { return nil },
		})
	}
	return actions
}

// decisionsIn extracts the drawn decisions from a journal, in order.
// Two runs with identical inputs must produce identical slices — that is
// the reproducibility contract.
func decisionsIn(t *testing.T, journal string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(journal), "\n") {
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("journal line is not valid JSON: %v (%q)", err, line)
		}
		if e.Event != evDecision {
			continue
		}
		out = append(out, strings.Join([]string{
			e.Action, e.Target,
			time.Duration(e.IntervalMS).String(),
			time.Duration(e.HoldMS).String(),
			e.Flip,
		}, "|"))
	}
	return out
}

func eventsIn(t *testing.T, journal string) []event {
	t.Helper()
	var out []event
	for _, line := range strings.Split(strings.TrimSpace(journal), "\n") {
		if line == "" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("journal line is not valid JSON: %v (%q)", err, line)
		}
		out = append(out, e)
	}
	return out
}

// =============================================================================
// Oracle lab fixture
// =============================================================================
//
// oracleLab is a consistent set of canned answers for every query the oracle
// makes, modelled on the everything-on profile: one router lr0 whose
// distributed LRP lr0-public is owned by gateway-1, three Gateway_Chassis
// candidates, two dnat_and_snat NATs, two managed static routes, and a flat
// provider segment. It is built so a "green" fixture really evaluates green —
// the observed data planes match what computeExpectation derives from the OVN
// tables — and tests mutate one field before running to diverge a single
// plane. Tests needing time-dependent behaviour (a plane that heals, a metric
// that climbs) wrap respond with their own counter.

// gwFIPs is the desired IP set the active gateway carries: the LRP gateway IP
// (192.0.2.1) and the two NAT external IPs, all on the flat br-ex segment.
var gwFIPs = []string{"192.0.2.1", "192.0.2.10", "192.0.2.12"}

type gcExtra struct {
	uuid, name, chassis string
	priority            int
}

type managedRoute struct {
	uuid, prefix, chassis, advertised string
}

type oracleLab struct {
	t *testing.T

	crOwner  string         // chassis owning cr-lr0-public
	priority map[string]int // Gateway_Chassis priority per gateway
	lrpGC    []string       // Gateway_Chassis row UUIDs on lr0-public
	extraGC  []gcExtra      // Gateway_Chassis rows beyond the three candidates
	managed  []managedRoute // managed static routes
	snapErr  bool           // fail the OVN snapshot on every poll
	marker   map[string]bool
	drainEnv string // printenv OVN_NETWORK_DRAIN_ON_SHUTDOWN answer

	dropKernel  map[string]string // gw → an IP to omit from proto-44 routes
	consecutive map[string]int    // gw → ovn_network_agent_consecutive_readds
	inactive    map[string]int    // gw → ovn_network_agent_inactive_routes
	readds      map[string]int    // gw → ovn_network_agent_route_readds_total

	// noVRFDefault drops the vrf-provider default route on a gateway. It moves
	// the scraped gauge with it, because the agent reads the same table the
	// observer does — a lab where only one of the two changed would be testing
	// a state the real one cannot reach.
	noVRFDefault map[string]bool
}

func newOracleLab(t *testing.T) *oracleLab {
	return &oracleLab{
		t:        t,
		crOwner:  "gateway-1",
		priority: map[string]int{"gateway-1": 30, "gateway-2": 20, "gateway-3": 10},
		lrpGC:    []string{"gc-gateway-1", "gc-gateway-2", "gc-gateway-3"},
		managed: []managedRoute{
			{uuid: "sr-10", prefix: "192.0.2.10/32", chassis: "gateway-1"},
			{uuid: "sr-12", prefix: "192.0.2.12/32", chassis: "gateway-1"},
		},
		marker:       map[string]bool{},
		drainEnv:     "false",
		dropKernel:   map[string]string{},
		consecutive:  map[string]int{},
		inactive:     map[string]int{},
		readds:       map[string]int{},
		noVRFDefault: map[string]bool{},
	}
}

func (o *oracleLab) respond(argv []string) (string, error) {
	line := strings.Join(argv, " ")
	has := func(s string) bool { return strings.Contains(line, s) }

	if o.snapErr && has("list Chassis") {
		return "", errBoom
	}
	switch {
	case has("find Port_Binding type=chassisredirect"):
		return o.portBinding(), nil
	case has("find Logical_Router_Static_Route"):
		return o.staticRoutes(), nil
	case has("list Logical_Router_Port"):
		return o.lrpTable(), nil
	case has("list Logical_Router"):
		return o.routerTable(), nil
	case has("list Logical_Switch_Port"):
		return o.lspTable(), nil
	case has("list Logical_Switch"):
		return o.lsTable(), nil
	case has("list Gateway_Chassis"):
		return o.gcTable(), nil
	case has("list NAT"):
		return o.natTable(), nil
	case has("list Chassis"):
		return o.chassisTable(), nil
	case has("show bgp ipv4 unicast json"):
		return o.upstreamBGP(), nil
	}

	if gw := gatewayOf(line); gw != "" {
		switch {
		case has("ip -j route show proto 44"):
			return o.kernel(gw), nil
		case has("show ip route vrf vrf-provider static json"):
			return o.frr(gw), nil
		case has("ip -j route show vrf vrf-provider default"):
			return o.vrfDefault(gw), nil
		case has("ip -j route show vrf vrf-provider proto 44"):
			return o.leak(gw), nil
		case has("show ip prefix-list ANNOUNCED-NETWORKS"):
			return o.prefixList(gw), nil
		case has("dump-flows br-ex cookie=0x998"):
			return o.hairpin(gw), nil
		case has("dump-flows br-ex cookie=0x999"):
			return o.macTweak(gw), nil
		case has("nft list table ip ovn-network-agent"):
			return "", errExit(o.t, 1) // table absent
		case has("ip -j addr show dev loopback1"):
			return "", errExit(o.t, 1) // device absent
		case has("printenv OVN_NETWORK_DRAIN_ON_SHUTDOWN"):
			return o.drainEnv + "\n", nil
		case has("test -f " + profileMarkerPath):
			if o.marker[gw] {
				return "", nil
			}
			return "", errExit(o.t, 1) // marker absent
		case has("/metrics"):
			return o.metrics(gw), nil
		}
	}
	return healthyLabResponses(argv)
}

// gatewayOf resolves which gateway container an argv targets.
func gatewayOf(line string) string {
	for _, gw := range gatewayNames() {
		if strings.Contains(line, "clab-ovn-e2e-"+gw) {
			return gw
		}
	}
	return ""
}

func (o *oracleLab) active(gw string) bool { return gw == o.crOwner }

// ---- OVN snapshot tables ----

func (o *oracleLab) chassisTable() string {
	rows := [][]any{}
	for _, gw := range gatewayNames() {
		rows = append(rows, []any{ovsUUID("ch-" + gw), gw})
	}
	return ovsTable([]string{"_uuid", "name"}, rows)
}

func (o *oracleLab) portBinding() string {
	chassis := any(ovsSet())
	if o.crOwner != "" {
		chassis = ovsUUID("ch-" + o.crOwner)
	}
	return ovsTable([]string{"logical_port", "chassis"},
		[][]any{{"cr-lr0-public", chassis}})
}

func (o *oracleLab) natTable() string {
	return ovsTable([]string{"_uuid", "external_ip", "type"}, [][]any{
		{ovsUUID("nat-10"), "192.0.2.10", "dnat_and_snat"},
		{ovsUUID("nat-12"), "192.0.2.12", "dnat_and_snat"},
	})
}

func (o *oracleLab) lrpTable() string {
	return ovsTable([]string{"_uuid", "name", "networks", "gateway_chassis"},
		[][]any{{ovsUUID("lrp-public"), "lr0-public", "192.0.2.1/24", gcRefs(o.lrpGC)}})
}

func (o *oracleLab) routerTable() string {
	return ovsTable([]string{"_uuid", "name", "ports", "nat"},
		[][]any{{ovsUUID("lr0"), "lr0", ovsUUID("lrp-public"),
			ovsSet(ovsUUID("nat-10"), ovsUUID("nat-12"))}})
}

func (o *oracleLab) gcTable() string {
	rows := [][]any{}
	for _, gw := range gatewayNames() {
		rows = append(rows, []any{ovsUUID("gc-" + gw), "lr0-public-" + gw, gw, o.priority[gw]})
	}
	for _, e := range o.extraGC {
		rows = append(rows, []any{ovsUUID(e.uuid), e.name, e.chassis, e.priority})
	}
	return ovsTable([]string{"_uuid", "name", "chassis_name", "priority"}, rows)
}

func (o *oracleLab) staticRoutes() string {
	rows := [][]any{}
	for _, m := range o.managed {
		ids := []string{"ovn-network-agent", "managed", "ovn-network-agent-chassis", m.chassis}
		if m.advertised != "" {
			ids = append(ids, "ovn-network-agent-advertised", m.advertised)
		}
		rows = append(rows, []any{ovsUUID(m.uuid), m.prefix, ovsMap(ids...)})
	}
	return ovsTable([]string{"_uuid", "ip_prefix", "external_ids"}, rows)
}

func (o *oracleLab) lsTable() string {
	return ovsTable([]string{"_uuid", "name", "ports"},
		[][]any{{ovsUUID("ls-public"), "public", ovsSet(ovsUUID("lsp-ln"), ovsUUID("lsp-router"))}})
}

func (o *oracleLab) lspTable() string {
	return ovsTable([]string{"_uuid", "name", "type", "tag", "options"}, [][]any{
		{ovsUUID("lsp-ln"), "ln-public", "localnet", ovsSet(), ovsMap("network_name", "physnet1")},
		{ovsUUID("lsp-router"), "public-lr0", "router", ovsSet(), ovsMap("router-port", "lr0-public")},
	})
}

// ---- per-gateway observations ----

func (o *oracleLab) kernel(gw string) string {
	arr := []map[string]string{}
	if o.active(gw) {
		for _, ip := range gwFIPs {
			if o.dropKernel[gw] == ip {
				continue
			}
			arr = append(arr, map[string]string{"dst": ip, "dev": "br-ex"})
		}
	}
	b, _ := json.Marshal(arr)
	return string(b)
}

func (o *oracleLab) frr(gw string) string {
	doc := map[string][]map[string]any{}
	if o.active(gw) {
		for _, ip := range gwFIPs {
			doc[ip+"/32"] = []map[string]any{{"nexthops": []map[string]string{{"ip": "169.254.0.1"}}}}
		}
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// leak answers the observer's `ip -j route show vrf vrf-provider proto 44`:
// the per-network veth-leak routes (#258). The fixture's docs run auto mode,
// where the leak set is the owned network — present on the active gateway,
// absent on a standby.
func (o *oracleLab) leak(gw string) string {
	if !o.active(gw) {
		return "[]"
	}
	return `[{"dst":"192.0.2.0/24","gateway":"169.254.0.1","dev":"veth-provider"}]`
}

// vrfDefault answers the observer's `ip -j route show vrf vrf-provider
// default`. Absence is an empty JSON array, which is what iproute2 prints.
func (o *oracleLab) vrfDefault(gw string) string {
	if o.noVRFDefault[gw] {
		return "[]"
	}
	link := mustLink(o.t, gw)
	return fmt.Sprintf(`[{"dst":"default","gateway":"%s","dev":"eth1","protocol":"bgp"}]`,
		addrOf(link.upstreamCIDR))
}

func (o *oracleLab) prefixList(gw string) string {
	if !o.active(gw) {
		return "" // the agent cleans the list on a standby gateway
	}
	return "ip prefix-list ANNOUNCED-NETWORKS: 1 entries\n" +
		"   seq 5 permit 192.0.2.0/24 ge 32 le 32\n"
}

func (o *oracleLab) hairpin(gw string) string {
	var b strings.Builder
	b.WriteString("NXST_FLOW reply (xid=0x4):\n")
	if o.active(gw) {
		for _, ip := range gwFIPs {
			fmt.Fprintf(&b, " cookie=0x998, table=0, priority=100,ip,nw_dst=%s actions=NORMAL\n", ip)
		}
	}
	return b.String()
}

func (o *oracleLab) macTweak(gw string) string {
	var b strings.Builder
	b.WriteString("NXST_FLOW reply (xid=0x4):\n")
	if o.active(gw) {
		b.WriteString(" cookie=0x999, table=0, priority=100,dl_vlan=0 actions=mod_dl_src\n")
		b.WriteString(" cookie=0x999, table=0, priority=100,dl_vlan=0 actions=mod_dl_dst\n")
	}
	return b.String()
}

func (o *oracleLab) metrics(gw string) string {
	vrfDefault := 1
	if o.noVRFDefault[gw] {
		vrfDefault = 0
	}
	return fmt.Sprintf("HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\n"+
		"# HELP ovn_network_agent_consecutive_readds ...\n"+
		"ovn_network_agent_consecutive_readds %d\n"+
		"ovn_network_agent_inactive_routes %d\n"+
		"ovn_network_agent_route_readds_total{plane=\"kernel\"} %d\n"+
		"ovn_network_agent_route_readds_total{plane=\"frr\"} 0\n"+
		"ovn_network_agent_vrf_default_route_present %d\n",
		o.consecutive[gw], o.inactive[gw], o.readds[gw], vrfDefault)
}

func (o *oracleLab) upstreamBGP() string {
	routes := map[string][]map[string]any{}
	if o.crOwner != "" {
		nexthop := addrOf(mustLink(o.t, o.crOwner).gatewayCIDR)
		for _, ip := range gwFIPs {
			routes[ip+"/32"] = []map[string]any{{"nexthops": []map[string]any{{"ip": nexthop}}}}
		}
	}
	b, _ := json.Marshal(map[string]any{"routes": routes})
	return string(b)
}

func mustLink(t *testing.T, gw string) underlayLink {
	t.Helper()
	link, ok := linkFor(gw)
	if !ok {
		t.Fatalf("no underlay link for %s", gw)
	}
	return link
}

// ---- OVSDB JSON builders ----

// ovsTable renders one `ovn-{n,s}bctl --format=json` table. Each cell is a
// scalar (string/int) or one of the ovs* wrappers below.
func ovsTable(headings []string, rows [][]any) string {
	if rows == nil {
		rows = [][]any{}
	}
	b, err := json.Marshal(map[string]any{"headings": headings, "data": rows})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func ovsUUID(id string) []any { return []any{"uuid", id} }

func ovsSet(items ...any) []any { return []any{"set", items} }

func ovsMap(kv ...string) []any {
	pairs := [][]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, []any{kv[i], kv[i+1]})
	}
	return []any{"map", pairs}
}

// gcRefs renders a Gateway_Chassis reference column: a bare UUID for one row,
// an OVSDB set for several, matching how ovsdb collapses single-element sets.
func gcRefs(uuids []string) any {
	if len(uuids) == 1 {
		return ovsUUID(uuids[0])
	}
	items := make([]any, 0, len(uuids))
	for _, u := range uuids {
		items = append(items, ovsUUID(u))
	}
	return ovsSet(items...)
}

// oracleApplier builds an applier carrying just the config the oracle reads:
// each gateway's current document and management address.
func oracleApplier(docs map[string]map[string]any) *applier {
	a := &applier{current: docs, mgmtIP: map[string]string{}}
	for _, gw := range gatewayNames() {
		a.mgmtIP[gw] = "172.20.20.4"
	}
	return a
}

// fullModeDocs parses the baked gwnode config into an independent document per
// gateway — the everything-on profile, where every gateway runs full OVN mode.
func fullModeDocs(t *testing.T) map[string]map[string]any {
	t.Helper()
	docs := map[string]map[string]any{}
	for _, gw := range gatewayNames() {
		doc, err := parseConfig(baseConfig(t))
		if err != nil {
			t.Fatalf("parse baked config: %v", err)
		}
		docs[gw] = doc
	}
	return docs
}
