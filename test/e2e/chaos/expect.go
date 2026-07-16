package main

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// =============================================================================
// Config-aware expected-state oracle
// =============================================================================
//
// computeExpectation recomputes, for one gateway, the exact data-plane state
// the agent must converge to — from a live OVN NB/SB snapshot and that
// gateway's own agent configuration. It is a pure function: no lab access, no
// polling, no side effects. The oracle's observation layer (a later commit)
// gathers the snapshot and polls the live planes against the verdict this
// returns.
//
// Everything here mirrors an agent code path, so the two derivations cannot
// drift: the desired-IP fork mirrors desired_state.go (computeDesiredState /
// DesiredIPs), and the per-mode / per-activity gating mirrors agent.go's
// reconcile branches (the port-forward-only path, the active branch that owns
// local routers, and the standby default branch). The rules are documented
// inline against the file they mirror rather than restated, so a reader can
// check the oracle against the agent one plane at a time.
//
// The oracle is IPv4-only, matching the agent's IPv4-only route/announce planes
// (desired_state.go:splitIPv4) and the lab, which seeds no IPv6 state.

// natRow is one flattened NB NAT row: only the two columns the oracle reads
// (the type distinguishes dnat_and_snat / snat, the external IP is what gets a
// route and an announcement).
type natRow struct {
	ExternalIP string
	Type       string
}

// lrpRow is a flattened NB Logical_Router_Port: its networks (CIDR strings, the
// gateway IPs the router owns) and the Gateway_Chassis row-UUID refs that make
// it a distributed gateway port.
type lrpRow struct {
	Networks       []string
	GatewayChassis []string
}

// routerRow is a flattened NB Logical_Router: the UUID refs of its member LRPs
// (resolved to names through ovnSnapshot.LRPNameByUUID) and its NAT rows.
type routerRow struct {
	LRPUUIDs []string
	NATs     []natRow
}

// gatewayChassisRow is one NB Gateway_Chassis row: the HA candidate a chassis
// offers for an LRP, at a priority. The oracle's observation layer reads these
// to reason about which chassis should win an election; computeExpectation
// takes the winner as already decided by the SB chassisredirect binding.
type gatewayChassisRow struct {
	Name        string
	ChassisName string
	Priority    int
}

// staticRouteRow is one managed NB Logical_Router_Static_Route: the observation
// layer matches it by its external_ids marker. computeExpectation does not read
// it — it is gathered here so the whole OVN view lives in one snapshot type.
type staticRouteRow struct {
	UUID        string
	IPPrefix    string
	ExternalIDs map[string]string
}

// ovnSnapshot is everything the oracle gathers from OVN NB/SB, already
// flattened to plain maps/slices so the observation layer can populate it from
// parseOVSDBTable output and the tests can hand-build it. It carries no
// libovsdb rows and no UUID indirection beyond LRPNameByUUID, which is the one
// join the router→LRP mapping needs.
type ovnSnapshot struct {
	// CRPortChassis maps each chassisredirect Port_Binding's logical port name
	// (e.g. "cr-lr0-public") to the chassis that currently owns it. An empty
	// value means the port is unbound (a re-election is in flight).
	CRPortChassis map[string]string

	// LRPs maps each Logical_Router_Port name to its flattened row.
	LRPs map[string]lrpRow

	// Routers maps each Logical_Router name to its flattened row.
	Routers map[string]routerRow

	// LRPNameByUUID resolves a Logical_Router_Port UUID to its name, so a
	// router's member-port UUID refs (routerRow.LRPUUIDs) connect to LRPs.
	LRPNameByUUID map[string]string

	// GatewayChassis maps each Gateway_Chassis row UUID to its row.
	GatewayChassis map[string]gatewayChassisRow

	// StaticRoutes are the managed Logical_Router_Static_Route rows.
	StaticRoutes []staticRouteRow

	// Chassis is the set of SB Chassis names currently present.
	Chassis map[string]bool

	// SegmentTagByLRP maps a Logical_Router_Port name to the VLAN tag of the
	// localnet segment its external network is on: 0 is a flat (untagged)
	// provider network, a non-zero tag is a VLAN segment. The oracle keys
	// segments by tag, which the lab's topology makes unambiguous — one flat
	// provider network at tag 0, each VLAN provider network at its own tag.
	SegmentTagByLRP map[string]int
}

// dnatExpectation is one expected nftables DNAT rule: VIP:port on the provider
// side, backend:destPort on the target side. Single-backend form only — the
// only shape the lab profiles produce.
type dnatExpectation struct {
	VIP      string
	Proto    string
	Port     int
	Backend  string
	DestPort int
}

// gatewayExpectation is the per-gateway verdict target: one field (or skip
// flag) per verified data plane. Each field is what the agent on this gateway
// must have converged to, given the OVN snapshot and its configuration.
type gatewayExpectation struct {
	// Mode is "full" (an OVN gateway) or "pf-only" (a standalone VIP service).
	Mode string

	// Active reports whether this gateway owns at least one chassisredirect
	// port — i.e. it is the active chassis for some router. Always false in
	// pf-only mode, where the agent holds no OVN view.
	Active bool

	// DesiredIPs is the sorted, deduplicated set of IPv4 addresses that need a
	// route and an announcement: the locally-active routers' NAT external IPs
	// (filtered by the effective networks) and LRP gateway IPs, plus the
	// configured port-forward VIPs. Mirrors desired_state.go DesiredIPs.
	DesiredIPs []string

	// KernelRouteDev maps each desired IP to the kernel device its proto-44
	// /32 route must sit on: the provider bridge for a flat segment or a VIP,
	// <bridge>.<tag> for a VLAN segment. Empty (and SkipKernel set) in pf-only.
	KernelRouteDev map[string]string
	SkipKernel     bool

	// FRRStatic is the set of /32 IPs expected as FRR static routes via the
	// veth nexthop. It equals DesiredIPs in both modes: ensureRoutes manages
	// FRR for every desired IP, active or standby, full or pf-only.
	FRRStatic []string

	// PrefixList is the sorted set of CIDR strings the ANNOUNCED-NETWORKS
	// prefix-list is reconciled to (the effective networks) on a full-mode
	// active gateway. nil means the list is expected absent — the agent cleans
	// it on a full-mode standby. SkipPrefixList is set in pf-only, where the
	// agent never touches the list (the entrypoint placeholder survives).
	PrefixList     []string
	SkipPrefixList bool

	// Hairpin is the set of IPs expected to carry a 0x998 hairpin flow: the
	// IPv4 hairpin targets (NAT external IPs + LRP gateway IPs) when active,
	// empty when standby. Port-forward VIPs are excluded — their DNAT is
	// nftables, not a hairpin (buildHairpinTargets). SkipHairpin is set in
	// pf-only.
	Hairpin     []string
	SkipHairpin bool

	// MACTweakFlows is the expected count of 0x999 MAC-tweak flows: two per
	// distinct locally-active segment when active. -1 means skip the check —
	// the agent does not clean MAC-tweak flows on standby, so a standby (or a
	// pf-only) gateway's count is unknowable.
	MACTweakFlows int

	// DNAT is the expected DNAT rule for each configured port-forward rule.
	// Empty when no port_forwards are configured (the agent's nftables table
	// then carries no DNAT chains).
	DNAT []dnatExpectation

	// HairpinMasquerade is the set of VIPs whose nftables ruleset must carry
	// the hairpin-masquerade rule (hairpin_masquerade set on the VIP and a
	// non-empty provider-network set, per buildNftRuleset's writeSNATChain).
	HairpinMasquerade []string

	// ManagedVIPs is the set of VIP addresses expected as /32 on
	// PortForwardDev, for VIPs with manage_vip on. Empty otherwise.
	ManagedVIPs    []string
	PortForwardDev string

	// MustAnnounce is the presence bound: the IPs the upstream must currently
	// be announcing. On a full-mode active gateway it is the desired IPs that
	// fall inside an effective network (the only ones the prefix-list permits);
	// in pf-only it is the VIPs (the entrypoint placeholder permits all /32s);
	// on a full-mode standby it is empty.
	MustAnnounce []string

	// AnnounceBound is the staleness bound: announced ⊆ this set must hold in
	// every mode and state. It is exactly the desired IPs — the upstream can
	// never legitimately announce an address the agent does not desire.
	AnnounceBound []string
}

// computeExpectation recomputes gw's expected data-plane state from the OVN
// snapshot and gw's agent configuration doc. mgmtIP is gw's management address
// (the backend its API VIP forwards to); the DNAT backend itself is read from
// the already-substituted config, so mgmtIP is threaded through only to keep
// one signature across the oracle's callers.
func computeExpectation(snap ovnSnapshot, gw string, doc map[string]any, mgmtIP string) gatewayExpectation {
	_ = mgmtIP

	// Rule 1 — pf-only iff both OVN remotes are absent (config.go validateMode:
	// port-forward-only = no remotes + port_forwards set).
	pfOnly := !docRemoteSet(doc, "ovn_sb_remote") && !docRemoteSet(doc, "ovn_nb_remote")
	mode := "full"
	if pfOnly {
		mode = "pf-only"
	}

	bridge := docString(doc, "bridge_dev", "br-ex")
	forwards := docPortForwards(doc)
	vips := vipAddresses(forwards)

	// In pf-only mode the agent runs with a zero OVNState (a.ovn is nil), so it
	// sees no local routers regardless of what OVN holds. Everything OVN-derived
	// below therefore reduces to the VIPs alone.
	var local []localRouter
	if !pfOnly {
		local = snap.localRoutersOn(gw)
	}
	active := len(local) > 0

	// Rule 2 — effective networks: the manual network_cidr wins, else the
	// networks discovered from the locally-active LRPs (config.go
	// effectiveNetworkFilters).
	effective := effectiveNetworks(doc, local)

	// Rules 3+4 — NAT external IPs are filtered by the effective networks (a FIP
	// outside every effective CIDR lands in no plane); LRP gateway IPs are kept
	// unfiltered. hairpin targets = NAT IPs + LRP gateway IPs; DesiredIPs adds
	// the VIPs.
	natIPs, lrpIPs, ipTag := ovnDesired(local, effective)
	hairpin := sortedUnique(append(append([]string{}, natIPs...), lrpIPs...))
	desired := sortedUnique(append(append([]string{}, hairpin...), vips...))

	exp := gatewayExpectation{
		Mode:           mode,
		Active:         active,
		DesiredIPs:     desired,
		FRRStatic:      desired, // rule 6 — FRR static routes are exactly the desired IPs, in both modes.
		PortForwardDev: docString(doc, "port_forward_dev", "loopback1"),
		MACTweakFlows:  -1,
		AnnounceBound:  desired, // rule 12 — announced ⊆ desired always.
	}

	// Rule 5 — kernel routes: managed only in full mode. Each OVN-derived IP
	// routes over its segment's device (<bridge>.<tag>, or the bridge for a
	// flat segment); VIPs default to the bridge. Skipped entirely in pf-only.
	if pfOnly {
		exp.SkipKernel = true
	} else {
		dev := make(map[string]string, len(desired))
		for _, ip := range desired {
			if tag, ok := ipTag[ip]; ok {
				dev[ip] = kernelDevForTag(bridge, tag)
			} else {
				dev[ip] = bridge
			}
		}
		exp.KernelRouteDev = dev
	}

	// Rule 7 — prefix-list: reconciled to the effective networks on a full-mode
	// active gateway, cleaned (nil) on a full-mode standby, never touched in
	// pf-only.
	switch {
	case pfOnly:
		exp.SkipPrefixList = true
	case active:
		exp.PrefixList = networkStrings(effective)
	}

	// Rule 8 — hairpin flows: the hairpin targets when active, removed on
	// standby, skipped in pf-only.
	switch {
	case pfOnly:
		exp.SkipHairpin = true
	case active:
		exp.Hairpin = hairpin
	}

	// Rule 9 — MAC-tweak flows: two per distinct locally-active segment when
	// active; left as the -1 skip sentinel on standby and in pf-only.
	if active {
		exp.MACTweakFlows = 2 * distinctSegments(local)
	}

	// Rules 10+11 — nftables. DNAT rules and managed-VIP addresses are managed
	// independently of OVN state (ReconcilePortForward runs every cycle), so
	// they follow the config alone.
	exp.DNAT = dnatExpectations(forwards)
	exp.HairpinMasquerade = hairpinMasqueradeVIPs(forwards, effective)
	exp.ManagedVIPs = managedVIPs(forwards)

	// Rule 12 — announced presence bound.
	switch {
	case pfOnly:
		exp.MustAnnounce = desired // = VIPs
	case active:
		exp.MustAnnounce = coveredBy(desired, effective)
	}

	return exp
}

// localRouter is one router whose chassisredirect port the target gateway owns,
// with the LRP that redirects to it resolved: its gateway IPs (networks), its
// segment's VLAN tag, and the router's NAT rows.
type localRouter struct {
	router   string
	lrpName  string
	networks []string
	segTag   int
	nats     []natRow
}

// localRoutersOn returns the routers gw is the active chassis for: those whose
// distributed gateway LRP has a chassisredirect port bound to gw. It mirrors
// the agent's SB→NB join (ovn.go localCRPorts → collectLocalRouters): a CR port
// owned by gw resolves to its LRP, and the LRP to the router that carries it.
func (s ovnSnapshot) localRoutersOn(gw string) []localRouter {
	var out []localRouter
	for crPort, owner := range s.CRPortChassis {
		if owner == "" || owner != gw {
			continue
		}
		lrpName := lrpOfCRPort(crPort)
		router, ok := s.routerForLRP(lrpName)
		if !ok {
			continue
		}
		out = append(out, localRouter{
			router:   router,
			lrpName:  lrpName,
			networks: s.LRPs[lrpName].Networks,
			segTag:   s.SegmentTagByLRP[lrpName],
			nats:     s.Routers[router].NATs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].router < out[j].router })
	return out
}

// routerForLRP returns the name of the Logical_Router that owns the named LRP,
// resolving each router's member-port UUID refs through LRPNameByUUID.
func (s ovnSnapshot) routerForLRP(lrpName string) (string, bool) {
	for router, r := range s.Routers {
		for _, uuid := range r.LRPUUIDs {
			if s.LRPNameByUUID[uuid] == lrpName {
				return router, true
			}
		}
	}
	return "", false
}

// lrpOfCRPort resolves the distributed LRP a chassisredirect port redirects for
// from the ovn-northd "cr-<LRP>" naming convention. That is the agent's own
// documented fallback (ovn.go crPortLRPName) when a Port_Binding carries no
// options:distributed-port, and it is the shape the lab's CR ports take
// (cr-lr0-public ↔ lr0-public). A name that does not fit resolves to no router
// and is skipped by the caller.
func lrpOfCRPort(crPort string) string {
	return strings.TrimPrefix(crPort, "cr-")
}

// ovnDesired collects the locally-active routers' route/announce-plane inputs:
// their NAT external IPs filtered by the effective networks (rule 3), their LRP
// gateway IPs kept unfiltered (rule 4), and the segment tag each IP sits on so
// the kernel-route device can be derived. All three are IPv4-only, matching the
// agent's route/announce planes.
//
// This mirrors the agent's addNBNATs derivation (ovn.go): NAT external IPs of
// locally-active routers, validated and filtered by the effective networks. It
// intentionally omits the agent's second NAT source, addSBGatewayNATAddresses,
// which folds in the SB gateway port's NatAddresses only for Neutron-managed
// ports (external_ids:neutron:device_owner == network:router_gateway). The lab
// seeds no neutron:device_owner metadata, so that path can never contribute and
// modelling it would add a snapshot column no lab row would ever populate.
func ovnDesired(local []localRouter, effective []*net.IPNet) (natIPs, lrpIPs []string, ipTag map[string]int) {
	ipTag = make(map[string]int)
	for _, lr := range local {
		for _, nat := range lr.nats {
			ip := net.ParseIP(nat.ExternalIP)
			if ip == nil || ip.To4() == nil {
				continue // invalid or non-IPv4: dropped at ingest, like addNBNATs + splitIPv4.
			}
			if len(effective) > 0 && !containedInAny(ip, effective) {
				continue // outside every effective CIDR — appears in no plane (rule 3).
			}
			natIPs = append(natIPs, ip.String())
			ipTag[ip.String()] = lr.segTag
		}
		for _, cidr := range lr.networks {
			ip, _, err := net.ParseCIDR(cidr)
			if err != nil || ip.To4() == nil {
				continue
			}
			lrpIPs = append(lrpIPs, ip.String())
			ipTag[ip.String()] = lr.segTag
		}
	}
	return natIPs, lrpIPs, ipTag
}

// distinctSegments counts the distinct localnet segments the locally-active
// routers span, keyed by VLAN tag. Two routers on the same provider segment
// share a tag and count once (mirrors desired_state.go buildDesiredSegments'
// dedup, which the lab's one-tag-per-segment topology makes equivalent).
func distinctSegments(local []localRouter) int {
	seen := make(map[int]bool, len(local))
	for _, lr := range local {
		seen[lr.segTag] = true
	}
	return len(seen)
}

// kernelDevForTag names the kernel device an IP's /32 route belongs on: the
// provider bridge for a flat (tag 0) segment, "<bridge>.<tag>" for a VLAN
// segment (mirrors routing_linux.go segmentIfaceName).
func kernelDevForTag(bridge string, tag int) string {
	if tag == 0 {
		return bridge
	}
	return fmt.Sprintf("%s.%d", bridge, tag)
}

// =============================================================================
// nftables / VIP derivations (config only)
// =============================================================================

// docPortForwardRule is one flattened port-forward rule from the config doc.
type docPortForwardRule struct {
	Proto    string
	Port     int
	DestAddr string
	DestPort int
}

// docPortForward is one flattened port_forwards entry from the config doc.
type docPortForward struct {
	VIP               string
	ManageVIP         bool
	HairpinMasquerade bool
	Rules             []docPortForwardRule
}

// docPortForwards narrows the doc's port_forwards block into the fields the
// oracle reads. It reuses vipsOf/vipAddrOf (flips.go) for the block and each
// entry's VIP, and reads the rules the config doc leaves as nested maps.
func docPortForwards(doc map[string]any) []docPortForward {
	var out []docPortForward
	for _, entry := range vipsOf(doc) {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		pf := docPortForward{
			VIP:               vipAddrOf(entry),
			ManageVIP:         boolValue(m["manage_vip"]),
			HairpinMasquerade: boolValue(m["hairpin_masquerade"]),
		}
		rules, _ := m["rules"].([]any)
		for _, r := range rules {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			proto, _ := rm["proto"].(string)
			addr, _ := rm["dest_addr"].(string)
			pf.Rules = append(pf.Rules, docPortForwardRule{
				Proto:    proto,
				Port:     intValue(rm["port"]),
				DestAddr: addr,
				DestPort: intValue(rm["dest_port"]),
			})
		}
		out = append(out, pf)
	}
	return out
}

// dnatExpectations renders one DNAT rule expectation per configured rule. The
// backend is the rule's dest_addr (the config the agent renders already carries
// the gateway's own mgmtIP there); the post-DNAT port defaults to the ingress
// port when the rule does not translate it (mirrors PortForwardRule.destPort).
func dnatExpectations(forwards []docPortForward) []dnatExpectation {
	var out []dnatExpectation
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			destPort := r.DestPort
			if destPort == 0 {
				destPort = r.Port
			}
			out = append(out, dnatExpectation{
				VIP:      pf.VIP,
				Proto:    r.Proto,
				Port:     r.Port,
				Backend:  r.DestAddr,
				DestPort: destPort,
			})
		}
	}
	return out
}

// hairpinMasqueradeVIPs returns the VIPs whose nftables ruleset carries the
// hairpin-masquerade rule. buildNftRuleset emits it only when the VIP has
// hairpin_masquerade set and there is at least one provider network to match a
// source against, so the effective-network set gates it too.
func hairpinMasqueradeVIPs(forwards []docPortForward, effective []*net.IPNet) []string {
	if len(effective) == 0 {
		return nil
	}
	var out []string
	for _, pf := range forwards {
		if pf.HairpinMasquerade {
			out = append(out, pf.VIP)
		}
	}
	return sortedUnique(out)
}

// managedVIPs returns the VIP addresses the agent adds as /32 on the port
// forward device, for VIPs with manage_vip on.
func managedVIPs(forwards []docPortForward) []string {
	var out []string
	for _, pf := range forwards {
		if pf.ManageVIP {
			out = append(out, pf.VIP)
		}
	}
	return sortedUnique(out)
}

// vipAddresses returns every configured VIP address.
func vipAddresses(forwards []docPortForward) []string {
	out := make([]string, 0, len(forwards))
	for _, pf := range forwards {
		out = append(out, pf.VIP)
	}
	return out
}

// =============================================================================
// Doc helpers
// =============================================================================

// docRemoteSet reports whether an OVN remote key is present and non-empty,
// matching validateMode's `cfg.OVNSBRemote != ""` test.
func docRemoteSet(doc map[string]any, key string) bool {
	s, _ := doc[key].(string)
	return s != ""
}

// docString reads a string key, falling back to def when absent or empty.
func docString(doc map[string]any, key, def string) string {
	if s, ok := doc[key].(string); ok && s != "" {
		return s
	}
	return def
}

// docDrainOnShutdown reports whether the config drains gateways on shutdown,
// treating an absent key as false — as the agent's config layering does. Used
// by the oracle's settle loop (a later commit) to decide how a terminated
// gateway is expected to hand its chassis over.
func docDrainOnShutdown(doc map[string]any) bool {
	return boolValue(doc["drain_on_shutdown"])
}

// =============================================================================
// Small pure helpers
// =============================================================================

// effectiveNetworks returns the network filters in effect: the manual
// network_cidr when set, else the networks discovered from the locally-active
// LRPs (config.go effectiveNetworkFilters). IPv4-only, matching the agent's
// computeEffectiveNetworks.
func effectiveNetworks(doc map[string]any, local []localRouter) []*net.IPNet {
	if manual := stringsOf(doc["network_cidr"]); len(manual) > 0 {
		return parseNetworks(manual)
	}
	var discovered []string
	for _, lr := range local {
		discovered = append(discovered, lr.networks...)
	}
	return parseNetworks(discovered)
}

// parseNetworks parses CIDR strings into their networks, dropping malformed and
// non-IPv4 entries and deduplicating by network.
func parseNetworks(cidrs []string) []*net.IPNet {
	var out []*net.IPNet
	seen := make(map[string]bool, len(cidrs))
	for _, s := range cidrs {
		_, n, err := net.ParseCIDR(s)
		if err != nil || n.IP.To4() == nil || seen[n.String()] {
			continue
		}
		seen[n.String()] = true
		out = append(out, n)
	}
	return out
}

// networkStrings renders the networks as sorted CIDR strings — the form the FRR
// prefix-list carries (net.IPNet.String()).
func networkStrings(nets []*net.IPNet) []string {
	out := make([]string, 0, len(nets))
	for _, n := range nets {
		out = append(out, n.String())
	}
	sort.Strings(out)
	return out
}

// containedInAny reports whether ip falls inside any of the networks.
func containedInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// coveredBy returns the subset of ips contained in some network, sorted. It is
// the presence bound on a full-mode active gateway: only desired IPs inside an
// effective network are permitted by the prefix-list and so must be announced.
func coveredBy(ips []string, nets []*net.IPNet) []string {
	var out []string
	for _, s := range ips {
		if ip := net.ParseIP(s); ip != nil && containedInAny(ip, nets) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// sortedUnique trims, deduplicates and sorts a list of strings.
func sortedUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// boolValue reads a bool from a config value, treating a missing key as false.
func boolValue(v any) bool {
	b, _ := v.(bool)
	return b
}

// intValue reads an int from a config value across the numeric types a YAML
// round-trip (or a hand-built doc) may produce.
func intValue(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}
