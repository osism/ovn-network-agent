package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// =============================================================================
// Oracle observation layer
// =============================================================================
//
// Everything here reads live lab state through the existing commander seam
// (lab.nbctl / lab.sbctl / lab.exec, and parseOVSDBTable's flattening in
// lab.go) and hands it back as the plain sets and maps the oracle (oracle.go)
// diffs against a config-aware expectation (expect.go). It computes nothing —
// it gathers, mirroring the agent code path each read verifies so the oracle
// checks one data plane at a time:
//
//   - snapshotOVN mirrors the SB→NB join the agent makes (ovn.go);
//   - observeGateway mirrors the FRR/kernel/OVS/nftables planes the agent
//     programs (routing.go, routing_linux.go, nftables.go, agent.go);
//   - observeUpstream reads the announcements the upstream BGP router actually
//     holds, the ground truth of what the underlay carries.
//
// IPv4-only, matching the agent's IPv4-only route/announce planes and the lab.

const (
	// ovnCtlTimeout bounds every ovn-{n,s}bctl the oracle issues, mirroring
	// crPortClaims in lab.go: the oracle reads a lab it is actively breaking,
	// and a wedged database read must fail fast, not hang the settle loop.
	ovnCtlTimeout = "--timeout=5"

	// vrfProvider is the VRF the agent installs its FRR statics into, and
	// vethNexthop the link-local next-hop it routes them via — both from the
	// baked gwnode config (test/e2e/gwnode-config.yaml). Only statics via this
	// next-hop are the agent's own (routing.go ListFRRRoutes).
	vrfProvider = "vrf-provider"
	vethNexthop = "169.254.0.1"

	// announcedPrefixList is the FRR prefix-list the agent reconciles to the
	// effective networks (config.go FRRPrefixList default).
	announcedPrefixList = "ANNOUNCED-NETWORKS"

	// bridgeDev is the provider bridge the agent programs its OVS flows on.
	// All lab gateways run on br-ex (gwnode-config.yaml bridge_dev).
	bridgeDev = "br-ex"

	// hairpinCookie / macTweakCookie are the OpenFlow cookies the agent stamps
	// its two flow classes with: 0x998 the hairpin DNAT flows, 0x999 the
	// MAC-tweak flows.
	hairpinCookie  = "cookie=0x998/-1"
	macTweakCookie = "cookie=0x999/-1"

	// agentNftTable is the nftables table the agent renders its port-forward
	// ruleset into (nftables.go nftTableName). A missing table is a legitimate
	// observation — the agent removes it when there is nothing to forward.
	agentNftTable = "ovn-network-agent"

	// The managed-route external_ids markers the agent stamps
	// (ovn_gateway.go): the managed marker, the owning chassis, and the
	// takeover-ready marker a takeover node sets before releasing its routes.
	managedRouteKey    = "ovn-network-agent"
	routeChassisKey    = "ovn-network-agent-chassis"
	routeAdvertisedKey = "ovn-network-agent-advertised"

	// metricsScrapeScript reads the agent's loopback-only Prometheus endpoint
	// (gwnode-config.yaml metrics_listen 127.0.0.1:9273) over a bash /dev/tcp
	// socket, since the gateway carries no HTTP client.
	metricsScrapeScript = `exec 3<>/dev/tcp/127.0.0.1/9273; printf "GET /metrics HTTP/1.0\r\n\r\n" >&3; cat <&3`
)

// =============================================================================
// OVN snapshot
// =============================================================================

// snapshotOVN gathers the whole OVN NB/SB view the oracle reasons about into
// one ovnSnapshot (expect.go). Every query mirrors crPortClaims in lab.go:
// bounded, --format=json, explicit --columns.
func snapshotOVN(ctx context.Context, l *lab) (ovnSnapshot, error) {
	snap := ovnSnapshot{
		CRPortChassis:   map[string]string{},
		LRPs:            map[string]lrpRow{},
		Routers:         map[string]routerRow{},
		LRPNameByUUID:   map[string]string{},
		GatewayChassis:  map[string]gatewayChassisRow{},
		Chassis:         map[string]bool{},
		SegmentTagByLRP: map[string]int{},
	}

	// SB Chassis: the present chassis set, and a UUID→name index for the
	// chassisredirect binding, whose chassis column is a UUID reference.
	chassisByUUID := map[string]string{}
	rows, err := sbRows(ctx, l, "Chassis", "", "_uuid", "name")
	if err != nil {
		return snap, err
	}
	for _, r := range rows {
		name := cellString(r["name"])
		if name == "" {
			continue
		}
		chassisByUUID[cellString(r["_uuid"])] = name
		snap.Chassis[name] = true
	}

	// SB chassisredirect Port_Binding: which chassis owns each cr port.
	rows, err = sbRows(ctx, l, "Port_Binding", "type=chassisredirect", "logical_port", "chassis")
	if err != nil {
		return snap, err
	}
	for _, r := range rows {
		port := cellString(r["logical_port"])
		if port == "" {
			continue
		}
		snap.CRPortChassis[port] = chassisByUUID[cellString(r["chassis"])]
	}

	// NB NAT: external IP and type, indexed by UUID for the router join.
	natByUUID := map[string]natRow{}
	rows, err = nbRows(ctx, l, "NAT", "", "_uuid", "external_ip", "type")
	if err != nil {
		return snap, err
	}
	for _, r := range rows {
		natByUUID[cellString(r["_uuid"])] = natRow{
			ExternalIP: cellString(r["external_ip"]),
			Type:       cellString(r["type"]),
		}
	}

	// NB Logical_Router_Port: networks and the Gateway_Chassis refs.
	rows, err = nbRows(ctx, l, "Logical_Router_Port", "", "_uuid", "name", "networks", "gateway_chassis")
	if err != nil {
		return snap, err
	}
	for _, r := range rows {
		name := cellString(r["name"])
		if name == "" {
			continue
		}
		snap.LRPNameByUUID[cellString(r["_uuid"])] = name
		snap.LRPs[name] = lrpRow{
			Networks:       cellStrings(r["networks"]),
			GatewayChassis: cellStrings(r["gateway_chassis"]),
		}
	}

	// NB Logical_Router: member LRP refs and NAT rows (resolved).
	rows, err = nbRows(ctx, l, "Logical_Router", "", "_uuid", "name", "ports", "nat")
	if err != nil {
		return snap, err
	}
	for _, r := range rows {
		name := cellString(r["name"])
		if name == "" {
			continue
		}
		var nats []natRow
		for _, u := range cellStrings(r["nat"]) {
			if n, ok := natByUUID[u]; ok {
				nats = append(nats, n)
			}
		}
		snap.Routers[name] = routerRow{LRPUUIDs: cellStrings(r["ports"]), NATs: nats}
	}

	// NB Gateway_Chassis: the HA candidates, keyed by row UUID.
	rows, err = nbRows(ctx, l, "Gateway_Chassis", "", "_uuid", "name", "chassis_name", "priority")
	if err != nil {
		return snap, err
	}
	for _, r := range rows {
		snap.GatewayChassis[cellString(r["_uuid"])] = gatewayChassisRow{
			Name:        cellString(r["name"]),
			ChassisName: cellString(r["chassis_name"]),
			Priority:    cellInt(r["priority"]),
		}
	}

	// NB Logical_Router_Static_Route: the agent-managed rows, matched by the
	// same external_ids marker its consumers use (ovn.go).
	rows, err = nbRows(ctx, l, "Logical_Router_Static_Route",
		"external_ids:"+managedRouteKey+"="+"managed", "_uuid", "ip_prefix", "external_ids")
	if err != nil {
		return snap, err
	}
	for _, r := range rows {
		snap.StaticRoutes = append(snap.StaticRoutes, staticRouteRow{
			UUID:        cellString(r["_uuid"]),
			IPPrefix:    cellString(r["ip_prefix"]),
			ExternalIDs: ovsdbMap(r["external_ids"]),
		})
	}

	if err := snapshotSegments(ctx, l, &snap); err != nil {
		return snap, err
	}
	return snap, nil
}

// snapshotSegments derives each LRP's provider-segment VLAN tag by joining the
// logical switches to their localnet and router ports: the localnet port
// carries the segment tag, the router port names the LRP it patches to.
func snapshotSegments(ctx context.Context, l *lab, snap *ovnSnapshot) error {
	type lspInfo struct {
		typ        string
		tag        int
		routerPort string
	}
	byUUID := map[string]lspInfo{}
	rows, err := nbRows(ctx, l, "Logical_Switch_Port", "", "_uuid", "name", "type", "tag", "options")
	if err != nil {
		return err
	}
	for _, r := range rows {
		byUUID[cellString(r["_uuid"])] = lspInfo{
			typ:        cellString(r["type"]),
			tag:        cellInt(r["tag"]),
			routerPort: ovsdbMap(r["options"])["router-port"],
		}
	}

	rows, err = nbRows(ctx, l, "Logical_Switch", "", "_uuid", "name", "ports")
	if err != nil {
		return err
	}
	for _, r := range rows {
		tag := 0
		var lrps []string
		for _, u := range cellStrings(r["ports"]) {
			p := byUUID[u]
			switch p.typ {
			case "localnet":
				tag = p.tag
			case "router":
				if p.routerPort != "" {
					lrps = append(lrps, p.routerPort)
				}
			}
		}
		for _, lrp := range lrps {
			snap.SegmentTagByLRP[lrp] = tag
		}
	}
	return nil
}

// nbRows / sbRows run one `list`/`find` against the NB/SB database and return
// the rows with each cell kept as raw JSON, so a map column (external_ids,
// options) can be parsed with ovsdbMap while a scalar/set/uuid column flattens
// through the shared flattenOVSDBCell.
func nbRows(ctx context.Context, l *lab, table, cond string, columns ...string) ([]map[string]json.RawMessage, error) {
	return ovnQuery(ctx, l.nbctl, table, cond, columns)
}

func sbRows(ctx context.Context, l *lab, table, cond string, columns ...string) ([]map[string]json.RawMessage, error) {
	return ovnQuery(ctx, l.sbctl, table, cond, columns)
}

func ovnQuery(ctx context.Context, ctl func(context.Context, ...string) (string, error),
	table, cond string, columns []string) ([]map[string]json.RawMessage, error) {
	args := []string{ovnCtlTimeout, "--format=json", "--columns=" + strings.Join(columns, ",")}
	if cond == "" {
		args = append(args, "list", table)
	} else {
		args = append(args, "find", table, cond)
	}
	out, err := ctl(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("read ovsdb table %s: %w", table, err)
	}
	rows, err := ovsdbRows(out)
	if err != nil {
		return nil, fmt.Errorf("parse ovsdb table %s: %w", table, err)
	}
	return rows, nil
}

// ovsdbRows decodes an ovn-{n,s}bctl --format=json table, keeping each cell as
// raw JSON. It is the raw-cell counterpart of parseOVSDBTable in lab.go, which
// flattens every cell to strings and so drops a map column's key/value pairs.
func ovsdbRows(out string) ([]map[string]json.RawMessage, error) {
	var table struct {
		Headings []string            `json:"headings"`
		Data     [][]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &table); err != nil {
		return nil, fmt.Errorf("decode ovsdb json: %w", err)
	}
	rows := make([]map[string]json.RawMessage, 0, len(table.Data))
	for _, data := range table.Data {
		row := map[string]json.RawMessage{}
		for i, heading := range table.Headings {
			if i < len(data) {
				row[heading] = data[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// ovsdbMap parses an OVSDB map cell (`["map",[[k,v],...]]`) into a plain
// string map. Anything else — a scalar, a set, an empty cell — is an empty
// map, so external_ids and options read cleanly whether or not they carry
// values.
func ovsdbMap(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return out
	}
	var kind string
	if err := json.Unmarshal(tuple[0], &kind); err != nil || kind != "map" {
		return out
	}
	var pairs [][]json.RawMessage
	if err := json.Unmarshal(tuple[1], &pairs); err != nil {
		return out
	}
	for _, p := range pairs {
		if len(p) != 2 {
			continue
		}
		var k, v string
		if json.Unmarshal(p[0], &k) == nil && json.Unmarshal(p[1], &v) == nil {
			out[k] = v
		}
	}
	return out
}

// cellString / cellStrings flatten a scalar/uuid/set cell through the shared
// flattenOVSDBCell; cellInt reads an integer column (priority, tag), which
// flattenOVSDBCell does not model.
func cellString(raw json.RawMessage) string {
	if s := flattenOVSDBCell(raw); len(s) > 0 {
		return s[0]
	}
	return ""
}

func cellStrings(raw json.RawMessage) []string {
	return flattenOVSDBCell(raw)
}

func cellInt(raw json.RawMessage) int {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	return 0
}

// =============================================================================
// Per-gateway observation
// =============================================================================

// observedGateway is the live data-plane state gathered from one gateway, in
// the same shape as gatewayExpectation so the oracle can diff a plane at a
// time.
type observedGateway struct {
	kernelRoutes     map[string]string // desired IP → kernel device
	frrStatic        map[string]bool   // /32 IPs with an agent FRR static
	prefixList       map[string]bool   // ANNOUNCED-NETWORKS CIDRs
	prefixListAbsent bool              // the list does not exist at all
	hairpin          map[string]bool   // IPs carrying a 0x998 hairpin flow
	macTweakFlows    int               // count of 0x999 MAC-tweak flows
	dnat             []observedDNAT    // nftables prerouting_dnat rules
	masquerade       map[string]bool   // VIPs with a hairpin/router masquerade rule
	vips             map[string]bool   // /32 addresses on the port-forward device
	metrics          gatewayMetrics
}

// observedDNAT is one nftables DNAT rule read back from the live ruleset.
type observedDNAT struct {
	vip      string
	proto    string
	port     int
	backend  string
	destPort int
}

// gatewayMetrics are the flap-indicator series the oracle gates on.
type gatewayMetrics struct {
	consecutiveReAdds int
	inactiveRoutes    int
	routeReAddsTotal  int // summed over the plane labels
}

// observeGateway gathers every data plane the oracle verifies on one gateway.
// portForwardDev is the device managed VIP addresses land on (loopback1 by
// default), read from the gateway's expectation.
func observeGateway(ctx context.Context, l *lab, gw, portForwardDev string) (observedGateway, error) {
	obs := observedGateway{
		kernelRoutes: map[string]string{},
		frrStatic:    map[string]bool{},
		prefixList:   map[string]bool{},
		hairpin:      map[string]bool{},
		masquerade:   map[string]bool{},
		vips:         map[string]bool{},
	}
	if err := observeKernelRoutes(ctx, l, gw, &obs); err != nil {
		return obs, err
	}
	if err := observeFRRStatic(ctx, l, gw, &obs); err != nil {
		return obs, err
	}
	if err := observePrefixList(ctx, l, gw, &obs); err != nil {
		return obs, err
	}
	if err := observeFlows(ctx, l, gw, &obs); err != nil {
		return obs, err
	}
	if err := observeNftables(ctx, l, gw, &obs); err != nil {
		return obs, err
	}
	if err := observeVIPs(ctx, l, gw, portForwardDev, &obs); err != nil {
		return obs, err
	}
	if err := observeMetrics(ctx, l, gw, &obs); err != nil {
		return obs, err
	}
	return obs, nil
}

// observeKernelRoutes reads the agent's proto-44 /32 routes and the device
// each sits on (routing_linux.go rtProtoOVNNetworkAgent).
func observeKernelRoutes(ctx context.Context, l *lab, gw string, obs *observedGateway) error {
	out, err := l.exec(ctx, gw, "ip", "-j", "route", "show", "proto", "44")
	if err != nil {
		return fmt.Errorf("list proto-44 routes on %s: %w", gw, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var routes []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return fmt.Errorf("parse proto-44 routes on %s: %w", gw, err)
	}
	for _, r := range routes {
		obs.kernelRoutes[strings.TrimSuffix(r.Dst, "/32")] = r.Dev
	}
	return nil
}

// observeFRRStatic reads the agent's own /32 FRR statics: those via the veth
// next-hop, mirroring routing.go ListFRRRoutes.
func observeFRRStatic(ctx context.Context, l *lab, gw string, obs *observedGateway) error {
	out, err := l.exec(ctx, gw, "vtysh", "-c",
		fmt.Sprintf("show ip route vrf %s static json", vrfProvider))
	if err != nil {
		return fmt.Errorf("list frr statics on %s: %w", gw, err)
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	var routes map[string][]struct {
		Nexthops []struct {
			IP string `json:"ip"`
		} `json:"nexthops"`
	}
	if err := json.Unmarshal([]byte(trimmed), &routes); err != nil {
		return fmt.Errorf("parse frr statics on %s: %w", gw, err)
	}
	for prefix, entries := range routes {
		if !strings.HasSuffix(prefix, "/32") {
			continue
		}
		for _, e := range entries {
			for _, nh := range e.Nexthops {
				if nh.IP == vethNexthop {
					obs.frrStatic[strings.TrimSuffix(prefix, "/32")] = true
				}
			}
		}
	}
	return nil
}

// observePrefixList reads the ANNOUNCED-NETWORKS prefix-list, keeping the CIDR
// of each `permit <cidr> ge 32 le 32` entry and distinguishing a list that
// does not exist at all (which the agent cleans on a standby gateway).
func observePrefixList(ctx context.Context, l *lab, gw string, obs *observedGateway) error {
	out, err := l.exec(ctx, gw, "vtysh", "-c", "show ip prefix-list "+announcedPrefixList)
	if err != nil {
		return fmt.Errorf("read prefix-list on %s: %w", gw, err)
	}
	obs.prefixList, obs.prefixListAbsent = parsePrefixList(out)
	return nil
}

// parsePrefixList extracts the permitted CIDRs from `show ip prefix-list`
// text. A body that names no list at all is reported absent.
func parsePrefixList(out string) (map[string]bool, bool) {
	cidrs := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "permit" {
				cidrs[fields[i+1]] = true
			}
		}
	}
	absent := len(cidrs) == 0 && !strings.Contains(out, announcedPrefixList)
	return cidrs, absent
}

var flowDstRE = regexp.MustCompile(`(?:nw_dst|ip_dst)=([0-9.]+)`)

// observeFlows reads the two OVS flow classes on the provider bridge: the
// 0x998 hairpin flows (by their IPv4 destination) and the 0x999 MAC-tweak
// flows (by count).
func observeFlows(ctx context.Context, l *lab, gw string, obs *observedGateway) error {
	hairpin, err := l.exec(ctx, gw, "ovs-ofctl", "dump-flows", bridgeDev, hairpinCookie)
	if err != nil {
		return fmt.Errorf("dump hairpin flows on %s: %w", gw, err)
	}
	for _, m := range flowDstRE.FindAllStringSubmatch(hairpin, -1) {
		obs.hairpin[m[1]] = true
	}

	macTweak, err := l.exec(ctx, gw, "ovs-ofctl", "dump-flows", bridgeDev, macTweakCookie)
	if err != nil {
		return fmt.Errorf("dump mac-tweak flows on %s: %w", gw, err)
	}
	for _, line := range strings.Split(macTweak, "\n") {
		if strings.Contains(line, "cookie=0x999") {
			obs.macTweakFlows++
		}
	}
	return nil
}

var (
	nftDNATRE = regexp.MustCompile(`ip daddr (\S+) (\S+) dport (\d+) dnat to (\S+):(\d+)`)
	nftMasqRE = regexp.MustCompile(`ct original (?:ip )?daddr (\S+) ct status dnat masquerade`)
)

// observeNftables reads the agent's port-forward ruleset. A missing table is a
// legitimate observation — the agent removes it when there is nothing to
// forward — and is reported as empty DNAT/masquerade sets, not an error.
//
// Only nft's own exit 1 carries that answer: the error errors.As inspects is
// the docker CLI's, not the remote command's (lab.exec → lab.docker →
// execCommander.run), so every non-zero docker exit is an *exec.ExitError too.
// Widening this to any exit code would read a stopped container, an
// unreachable daemon or an expired cmdTimeout as an empty ruleset — and on a
// gateway configured with no port forwards the oracle would diff empty against
// empty and report three data planes converged having never read them. The
// same exit-1-is-an-answer split agentAlive and checkConfig make.
func observeNftables(ctx context.Context, l *lab, gw string, obs *observedGateway) error {
	out, err := l.exec(ctx, gw, "nft", "list", "table", "ip", agentNftTable)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil // the table does not exist — nothing to forward
		}
		return fmt.Errorf("read nft table on %s: %w", gw, err)
	}
	for _, m := range nftDNATRE.FindAllStringSubmatch(out, -1) {
		obs.dnat = append(obs.dnat, observedDNAT{
			vip:      m[1],
			proto:    m[2],
			port:     atoi(m[3]),
			backend:  m[4],
			destPort: atoi(m[5]),
		})
	}
	for _, m := range nftMasqRE.FindAllStringSubmatch(out, -1) {
		obs.masquerade[m[1]] = true
	}
	return nil
}

// observeVIPs reads the /32 addresses the agent manages on the port-forward
// device. An absent device is an empty set, not an error — and, as in
// observeNftables, only `ip`'s own exit 1 is that answer.
func observeVIPs(ctx context.Context, l *lab, gw, dev string, obs *observedGateway) error {
	out, err := l.exec(ctx, gw, "ip", "-j", "addr", "show", "dev", dev)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil // the device does not exist — no managed VIP addresses
		}
		return fmt.Errorf("list addresses on %s dev %s: %w", gw, dev, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var links []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return fmt.Errorf("parse addresses on %s dev %s: %w", gw, dev, err)
	}
	for _, link := range links {
		for _, a := range link.AddrInfo {
			if a.PrefixLen == 32 {
				obs.vips[a.Local] = true
			}
		}
	}
	return nil
}

// observeMetrics scrapes the agent's loopback Prometheus endpoint for the
// flap-indicator series.
func observeMetrics(ctx context.Context, l *lab, gw string, obs *observedGateway) error {
	out, err := l.exec(ctx, gw, "bash", "-c", metricsScrapeScript)
	if err != nil {
		return fmt.Errorf("scrape metrics on %s: %w", gw, err)
	}
	obs.metrics = parseMetrics(out)
	return nil
}

// parseMetrics reads the three flap-indicator series from a Prometheus text
// scrape (metrics.go), summing route_readds_total across its plane labels.
func parseMetrics(body string) gatewayMetrics {
	var m gatewayMetrics
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		value := parseMetricValue(fields[len(fields)-1])
		switch {
		case name == "ovn_network_agent_consecutive_readds":
			m.consecutiveReAdds = value
		case name == "ovn_network_agent_inactive_routes":
			m.inactiveRoutes = value
		case strings.HasPrefix(name, "ovn_network_agent_route_readds_total"):
			m.routeReAddsTotal += value
		}
	}
	return m
}

func parseMetricValue(s string) int {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// =============================================================================
// Upstream announcements
// =============================================================================

// upstreamRoutes maps each gateway-attributed announced prefix to the set of
// gateways announcing it, as read from the upstream BGP router. A /32 is keyed
// by its bare IP (matching AnnounceBound, which holds bare /32 IPs); any other
// prefix keeps its full CIDR string, so a leaked non-/32 (an underlay /30 a
// mis-filtered redistribute connected would export) lands in the "unexpected"
// half of the oracle's announced check (#223).
type upstreamRoutes map[string]map[string]bool

// announcedBy returns the prefixes gw is currently announcing upstream, keyed
// as upstreamRoutes documents (bare IP for a /32, full CIDR otherwise).
func (u upstreamRoutes) announcedBy(gw string) map[string]bool {
	out := map[string]bool{}
	for ip, gws := range u {
		if gws[gw] {
			out[ip] = true
		}
	}
	return out
}

// observeUpstream reads what the upstream BGP router actually holds — the
// ground truth of what the underlay carries — and attributes each /32 to the
// gateway that announced it by mapping the path's next-hop through the
// underlay links (gateway N announces from 100.64.N.2).
func observeUpstream(ctx context.Context, l *lab) (upstreamRoutes, error) {
	out, err := l.exec(ctx, upstreamNode, "vtysh", "-c", "show bgp ipv4 unicast json")
	if err != nil {
		return nil, fmt.Errorf("read upstream bgp: %w", err)
	}
	var doc struct {
		Routes map[string][]struct {
			Nexthops []struct {
				IP string `json:"ip"`
			} `json:"nexthops"`
			Peer *struct {
				PeerID string `json:"peerId"`
			} `json:"peer"`
		} `json:"routes"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, fmt.Errorf("parse upstream bgp: %w", err)
	}

	byNexthop := map[string]string{}
	for _, link := range underlayLinks {
		byNexthop[addrOf(link.gatewayCIDR)] = link.gateway
	}

	routes := upstreamRoutes{}
	for prefix, paths := range doc.Routes {
		// A /32 keeps its bare-IP key (so it matches AnnounceBound); any other
		// prefix keeps its full CIDR string, so a gateway-attributed non-/32 —
		// an underlay /30 a bare `redistribute connected` would leak — is
		// recorded and flagged as unexpected by the announce bound. Only
		// gateway-attributed prefixes land here at all: an upstream-originated
		// route resolves to no gateway (byNexthop maps only gateway addresses)
		// and stays invisible, as before.
		key := prefix
		if strings.HasSuffix(prefix, "/32") {
			key = strings.TrimSuffix(prefix, "/32")
		}
		for _, p := range paths {
			addrs := make([]string, 0, len(p.Nexthops)+1)
			for _, nh := range p.Nexthops {
				addrs = append(addrs, nh.IP)
			}
			if p.Peer != nil {
				addrs = append(addrs, p.Peer.PeerID)
			}
			for _, addr := range addrs {
				if gw := byNexthop[addr]; gw != "" {
					if routes[key] == nil {
						routes[key] = map[string]bool{}
					}
					routes[key][gw] = true
				}
			}
		}
	}
	return routes, nil
}

// sortedKeys returns a map's keys in sorted order, so an iteration over the
// snapshot's maps yields deterministic violation ordering.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
