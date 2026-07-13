package main

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
)

const (
	nftTableName       = "ovn-network-agent"
	dnatFwmark         = 0x100 // fwmark on original-direction DNAT'd packets → lookup main
	dnatReplyFwmark    = 0x200 // fwmark on reply-direction DNAT'd packets → lookup VRF
	dnatFwmarkPriority = 150   // ip rule priority; must be < 1000 (l3mdev VRF rule)
	dnatReplyPriority  = 151   // ip rule priority for reply return path
)

// buildNftRuleset generates the complete nftables ruleset for port forwarding.
// It produces up to eight chains:
//   - prerouting_ctzone: assigns a shared conntrack zone to both directions
//     of DNAT'd flows (runs at raw priority, before conntrack)
//   - output_ctzone: mirrors prerouting_ctzone for locally generated packets
//     (needed when DNAT backends run on the same host)
//   - prerouting_dnat: DNAT rules for each VIP:port → backend
//   - prerouting_fwmark: marks DNAT'd packets for policy routing
//   - output_fwmark: mirrors reply-direction fwmark for locally generated
//     replies (needed when DNAT backends run on the same host)
//   - forward_veth_guard: whitelist-based security for veth return path
//   - postrouting_fwmark_clear: clears reply fwmark before veth crossing
//   - postrouting_snat: masquerade for masquerade-enabled VIPs (optional)
//
// The conntrack zone is critical: DNAT'd traffic crosses VRF boundaries
// (original arrives on provider/VRF, reply arrives on control-plane/default VRF).
// Without a shared zone, conntrack cannot match the reply to the original
// connection and the reverse NAT fails silently.
//
// The output_ctzone and output_fwmark chains handle the case where a DNAT
// backend runs on the same host. When a packet is DNAT'd to a local address,
// it is delivered via INPUT (not FORWARD), so the reply originates from OUTPUT
// instead of PREROUTING. Without these output chains, conntrack cannot find
// the DNAT entry (wrong zone) and the reply is never policy-routed back
// through the veth pair into the provider VRF.
//
// snatIPs contains the router SNAT external IPs discovered from OVN (type=snat
// NAT entries). These are used by the router_masquerade feature to generate
// targeted SNAT rules for traffic from instances behind a router without a FIP.
//
// Safety: all interpolated values (VIPs, protocols, dest addresses) must be
// pre-validated by validateConfig before reaching this function.
// nftTablesDoc is the subset of an `nft -j list tables` document the agent
// inspects. nft emits one object per element, of which only the "table" ones
// carry a family/name pair.
type nftTablesDoc struct {
	Nftables []struct {
		Table *struct {
			Family string `json:"family"`
			Name   string `json:"name"`
		} `json:"table"`
	} `json:"nftables"`
}

// nftHasTable reports whether an `nft -j list tables` document lists the given
// table. This is the structured existence check that replaces deleting blindly
// and pattern-matching the wording of the resulting error: "No such file" is
// nft's phrasing, not a contract, so a reworded message would turn a clean
// teardown into a reported failure — or, worse, mask a real one.
func nftHasTable(data []byte, family, name string) (bool, error) {
	var doc nftTablesDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse nft json: %w", err)
	}
	for _, e := range doc.Nftables {
		if e.Table != nil && e.Table.Family == family && e.Table.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// snatEntry describes one backend destination that needs SNAT. Per-rule
// granularity is essential when a VIP has both local and remote backends: local
// backends must NOT be masqueraded (the reply originates locally and is handled
// by the output chains), while remote backends MUST be masqueraded so their
// replies return to this node for reverse NAT.
type snatEntry struct {
	addr  string // backend dest address (post-DNAT)
	proto string
	port  int // backend dest port (post-DNAT)
}

// destPort returns a rule's post-DNAT destination port, which defaults to the
// ingress port when the rule does not translate it.
func (r PortForwardRule) destPort() int {
	if r.DestPort == 0 {
		return r.Port
	}
	return r.DestPort
}

// collectVIPs returns every VIP address, in configuration order.
func collectVIPs(forwards []PortForwardVIP) []string {
	vips := make([]string, 0, len(forwards))
	for _, pf := range forwards {
		vips = append(vips, pf.VIP)
	}
	return vips
}

// collectSNATEntries returns one entry per backend destination whose rule has
// masquerade in effect. A rule inherits the VIP-level masquerade flag unless it
// overrides it (see effectiveMasquerade).
func collectSNATEntries(forwards []PortForwardVIP) []snatEntry {
	var entries []snatEntry
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			if !r.effectiveMasquerade(pf.Masquerade) {
				continue
			}
			for _, addr := range r.destAddrs() {
				entries = append(entries, snatEntry{addr: addr, proto: r.Proto, port: r.destPort()})
			}
		}
	}
	return entries
}

// collectHairpinVIPs returns the VIPs with hairpin_masquerade enabled. Hairpin
// masquerade applies SNAT only to traffic from provider networks so that the
// backend always replies through this node — solving the hairpin NAT problem
// where a FIP on the same node connects to a port-forwarded VIP. Unlike the
// VIP-level masquerade (which masquerades all traffic), this is source-selective.
func collectHairpinVIPs(forwards []PortForwardVIP) []string {
	var vips []string
	for _, pf := range forwards {
		if pf.HairpinMasquerade {
			vips = append(vips, pf.VIP)
		}
	}
	return vips
}

// collectRouterMasqVIPs returns the VIPs with router_masquerade enabled. Router
// masquerade applies SNAT only to traffic whose source is a known router SNAT IP
// (discovered from OVN NB NAT entries of type "snat"). This solves the hairpin
// NAT problem for instances behind a router that have no Floating IP: the router
// SNAT'd source IP is an OVN-managed address, so without masquerade the backend's
// reply enters OVN's pipeline directly — bypassing this node's conntrack — and
// the reverse DNAT never fires. Unlike hairpin_masquerade (which uses the full
// provider CIDR), this is more surgical: only the specific known SNAT IPs are
// masqueraded, leaving all other external clients' IPs fully preserved.
func collectRouterMasqVIPs(forwards []PortForwardVIP) []string {
	var vips []string
	for _, pf := range forwards {
		if pf.RouterMasquerade {
			vips = append(vips, pf.VIP)
		}
	}
	return vips
}

// netStrings renders the provider networks as CIDR strings.
func netStrings(nets []*net.IPNet) []string {
	out := make([]string, len(nets))
	for i, n := range nets {
		out[i] = n.String()
	}
	return out
}

// setMatch renders a single value bare and multiple values as an anonymous nft
// set, which is what the SNAT source matches expect.
func setMatch(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return "{ " + strings.Join(values, ", ") + " }"
}

// ctOriginalDaddrMatch renders the "original destination is one of our VIPs"
// match used by the fwmark chains.
//
// nft note: for a single bare-IP literal, `ct original daddr` can infer the
// address family. For an anonymous set with multiple values it cannot, and
// rejects with "specify either ip or ip6 for address matching". The explicit
// `ip daddr` pins the family so the set parses.
func ctOriginalDaddrMatch(vips []string) string {
	if len(vips) == 1 {
		return fmt.Sprintf("ct original daddr %s", vips[0])
	}
	return fmt.Sprintf("ct original ip daddr { %s }", strings.Join(vips, ", "))
}

// writeCTZoneRules emits conntrack zone assignment rules for both directions of
// DNAT'd flows. Shared by prerouting_ctzone and output_ctzone to keep their rule
// sets in sync.
func writeCTZoneRules(b *strings.Builder, forwards []PortForwardVIP, ctZone int) {
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			addrs := r.destAddrs()
			if len(addrs) == 0 {
				continue
			}
			destPort := r.destPort()
			fmt.Fprintf(b, "        ip daddr %s %s dport %d ct zone set %d\n",
				pf.VIP, r.Proto, r.Port, ctZone)
			for _, addr := range addrs {
				fmt.Fprintf(b, "        ip saddr %s %s sport %d ct zone set %d\n",
					addr, r.Proto, destPort, ctZone)
			}
		}
	}
}

// writeCTZoneChain emits one conntrack-zone chain with the given name and hook
// declaration.
//
// The conntrack zone is critical: DNAT'd traffic crosses VRF boundaries
// (original arrives on provider/VRF, reply arrives on control-plane/default VRF).
// Without a shared zone, conntrack cannot match the reply to the original
// connection and the reverse NAT fails silently. Priority raw (-300) runs BEFORE
// conntrack (-200), which is what makes the zone assignment take effect.
func writeCTZoneChain(b *strings.Builder, name, hookDecl string, forwards []PortForwardVIP, ctZone int) {
	fmt.Fprintf(b, "    chain %s {\n", name)
	fmt.Fprintf(b, "        %s\n", hookDecl)
	writeCTZoneRules(b, forwards, ctZone)
	b.WriteString("    }\n")
}

// writeDNATChain emits the DNAT rules.
// For rules with a single backend: direct DNAT.
// For rules with multiple backends: jhash on source IP for sticky load
// balancing — the same client IP always maps to the same backend.
func writeDNATChain(b *strings.Builder, forwards []PortForwardVIP) {
	b.WriteString("    chain prerouting_dnat {\n")
	b.WriteString("        type nat hook prerouting priority dstnat; policy accept;\n")
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			addrs := r.destAddrs()
			if len(addrs) == 0 {
				continue
			}
			destPort := r.destPort()
			if len(addrs) == 1 {
				fmt.Fprintf(b, "        ip daddr %s %s dport %d dnat to %s:%d\n",
					pf.VIP, r.Proto, r.Port, addrs[0], destPort)
				continue
			}
			// jhash: consistent source-IP hashing distributes clients
			// across backends. Same client IP → same backend (sticky).
			//
			// nft note: inside a verdict map for `dnat to`, the
			// (addr, port) target must use the concat operator
			// (`addr . port`), not the inline `addr:port` form. The
			// inline form is only valid for a single non-mapped
			// dnat target.
			fmt.Fprintf(b, "        ip daddr %s %s dport %d dnat to jhash ip saddr mod %d map { ",
				pf.VIP, r.Proto, r.Port, len(addrs))
			for i, addr := range addrs {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(b, "%d : %s . %d", i, addr, destPort)
			}
			b.WriteString(" }\n")
		}
	}
	b.WriteString("    }\n")
}

// writeFwmarkChains emits the two chains that policy-route DNAT'd packets.
//
// prerouting_fwmark runs at priority filter (0), after dstnat (-100), so
// `ct status dnat` is visible. Two fwmarks steer traffic into different routing
// tables:
//   - Original direction (client→backend): fwmark 0x100 → lookup main so the
//     packet escapes the VRF and reaches the backend via the default VRF's
//     routing (e.g. control-plane network).
//   - Reply direction (backend→client): fwmark 0x200 → lookup VRF so the
//     response exits via the provider network where the VIP source address is
//     legitimate (not filtered as spoofed).
//
// output_fwmark mirrors the reply-direction mark for locally generated replies:
// when a DNAT backend runs on the same host, reply traffic originates in OUTPUT
// (not PREROUTING). It uses "type route" so the mark change triggers a routing
// re-evaluation, steering the reply through the veth pair back into the provider
// VRF.
func writeFwmarkChains(b *strings.Builder, allVIPs []string) {
	daddr := ctOriginalDaddrMatch(allVIPs)

	b.WriteString("    chain prerouting_fwmark {\n")
	b.WriteString("        type filter hook prerouting priority filter; policy accept;\n")
	fmt.Fprintf(b, "        ct direction original ct status dnat %s meta mark set 0x%x\n", daddr, dnatFwmark)
	fmt.Fprintf(b, "        ct direction reply ct status dnat %s meta mark set 0x%x\n", daddr, dnatReplyFwmark)
	b.WriteString("    }\n")

	b.WriteString("    chain output_fwmark {\n")
	b.WriteString("        type route hook output priority filter; policy accept;\n")
	fmt.Fprintf(b, "        ct direction reply ct status dnat %s meta mark set 0x%x\n", daddr, dnatReplyFwmark)
	b.WriteString("    }\n")
}

// writeVethGuardChain emits the veth forward guard: it whitelists legitimate
// veth-leak return traffic and drops everything else going backwards through the
// veth pair.
func writeVethGuardChain(b *strings.Builder, providerNetworks []*net.IPNet) {
	b.WriteString("    chain forward_veth_guard {\n")
	b.WriteString("        type filter hook forward priority filter; policy accept;\n")
	// Allow existing veth leak return traffic (source in provider networks).
	if len(providerNetworks) > 0 {
		fmt.Fprintf(b, "        oifname \"%s\" ip saddr { %s } accept\n",
			vethDefaultName, strings.Join(netStrings(providerNetworks), ", "))
	}
	// Allow DNAT reply traffic returning to the VRF via veth pair.
	fmt.Fprintf(b, "        oifname \"%s\" meta mark 0x%x accept\n", vethDefaultName, dnatReplyFwmark)
	// Drop everything else going backwards through veth.
	fmt.Fprintf(b, "        oifname \"%s\" drop\n", vethDefaultName)
	b.WriteString("    }\n")
}

// writeFwmarkClearChain clears the DNAT reply fwmark before packets cross the
// veth pair. Without this the fwmark persists into the provider VRF and the ip
// rule matches again, creating a routing loop (veth-default → veth-provider →
// table 201 → veth-default → …).
func writeFwmarkClearChain(b *strings.Builder) {
	b.WriteString("    chain postrouting_fwmark_clear {\n")
	b.WriteString("        type filter hook postrouting priority filter; policy accept;\n")
	fmt.Fprintf(b, "        oifname \"%s\" meta mark 0x%x meta mark set 0\n",
		vethDefaultName, dnatReplyFwmark)
	b.WriteString("    }\n")
}

// writeSNATChain emits the postrouting SNAT chain, which covers three cases:
//
//  1. Per-rule masquerade (masquerade: true on VIP or rule): matches on the
//     post-DNAT destination (backend address). Local backends must NOT be
//     masqueraded — their replies are handled by the output chains instead.
//     Uses interface masquerade so replies from remote backends always return
//     to this node.
//
//  2. Hairpin masquerade (hairpin_masquerade: true on VIP): matches on the
//     pre-DNAT source address being within a provider network. Solves the
//     hairpin NAT problem: when a FIP in the provider network connects to a
//     VIP on the same node, the backend would otherwise see the FIP as source
//     and may route the reply asymmetrically (not back through this node).
//     By masquerading only provider-sourced traffic, non-hairpin clients are
//     unaffected.
//
//  3. Router masquerade (router_masquerade: true on VIP): matches on the
//     pre-DNAT source address being a known router SNAT IP. More surgical than
//     hairpin_masquerade: only specific SNAT IPs are masqueraded, leaving
//     external clients' IPs fully preserved.
//
// The chain is omitted entirely when none of the three applies.
func writeSNATChain(
	b *strings.Builder,
	snatEntries []snatEntry,
	hairpinVIPs, routerMasqVIPs []string,
	providerNetworks []*net.IPNet,
	snatIPs []string,
) {
	hairpinNeeded := len(hairpinVIPs) > 0 && len(providerNetworks) > 0
	routerMasqNeeded := len(routerMasqVIPs) > 0 && len(snatIPs) > 0
	if len(snatEntries) == 0 && !hairpinNeeded && !routerMasqNeeded {
		return
	}

	b.WriteString("    chain postrouting_snat {\n")
	b.WriteString("        type nat hook postrouting priority srcnat; policy accept;\n")
	for _, e := range snatEntries {
		fmt.Fprintf(b, "        ip daddr %s %s dport %d ct status dnat masquerade\n",
			e.addr, e.proto, e.port)
	}
	if hairpinNeeded {
		srcMatch := setMatch(netStrings(providerNetworks))
		for _, vip := range hairpinVIPs {
			fmt.Fprintf(b, "        ip saddr %s ct original daddr %s ct status dnat masquerade\n",
				srcMatch, vip)
		}
	}
	if routerMasqNeeded {
		sortedSNATIPs := append([]string{}, snatIPs...)
		sort.Strings(sortedSNATIPs)
		sortedSNATIPs = slices.Compact(sortedSNATIPs)
		srcMatch := setMatch(sortedSNATIPs)
		for _, vip := range routerMasqVIPs {
			fmt.Fprintf(b, "        ip saddr %s ct original daddr %s ct status dnat masquerade\n",
				srcMatch, vip)
		}
	}
	b.WriteString("    }\n")
}

// buildNftRuleset generates the complete nftables ruleset for port forwarding.
// It assembles up to eight chains, in this order:
//
//   - prerouting_ctzone: assigns a shared conntrack zone to both directions
//     of DNAT'd flows (runs at raw priority, before conntrack)
//   - output_ctzone: mirrors prerouting_ctzone for locally generated packets
//     (needed when DNAT backends run on the same host)
//   - prerouting_dnat: DNAT rules for each VIP:port → backend
//   - prerouting_fwmark: marks DNAT'd packets for policy routing
//   - output_fwmark: mirrors reply-direction fwmark for locally generated
//     replies (needed when DNAT backends run on the same host)
//   - forward_veth_guard: whitelist-based security for veth return path
//   - postrouting_fwmark_clear: clears reply fwmark before veth crossing
//   - postrouting_snat: masquerade for masquerade-enabled VIPs (optional)
//
// Chain order is the contract; each emitter above documents its own rationale.
//
// snatIPs contains the router SNAT external IPs discovered from OVN (type=snat
// NAT entries). These are used by the router_masquerade feature to generate
// targeted SNAT rules for traffic from instances behind a router without a FIP.
//
// Safety: all interpolated values (VIPs, protocols, dest addresses) must be
// pre-validated by validateConfig before reaching this function.
func buildNftRuleset(forwards []PortForwardVIP, providerNetworks []*net.IPNet, snatIPs []string, ctZone int) string {
	allVIPs := collectVIPs(forwards)
	snatEntries := collectSNATEntries(forwards)
	hairpinVIPs := collectHairpinVIPs(forwards)
	routerMasqVIPs := collectRouterMasqVIPs(forwards)

	var b strings.Builder
	fmt.Fprintf(&b, "table ip %s {\n", nftTableName)

	writeCTZoneChain(&b, "prerouting_ctzone",
		"type filter hook prerouting priority raw; policy accept;", forwards, ctZone)
	writeCTZoneChain(&b, "output_ctzone",
		"type filter hook output priority raw; policy accept;", forwards, ctZone)
	writeDNATChain(&b, forwards)
	writeFwmarkChains(&b, allVIPs)
	writeVethGuardChain(&b, providerNetworks)
	writeFwmarkClearChain(&b)
	writeSNATChain(&b, snatEntries, hairpinVIPs, routerMasqVIPs, providerNetworks, snatIPs)

	b.WriteString("}\n")
	return b.String()
}
