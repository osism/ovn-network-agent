package main

import (
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
	dnatFwmarkPriority = 150 // ip rule priority; must be < 1000 (l3mdev VRF rule)
	dnatReplyPriority  = 151 // ip rule priority for reply return path
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
func buildNftRuleset(forwards []PortForwardVIP, providerNetworks []*net.IPNet, snatIPs []string, ctZone int) string {
	// writeCTZoneRules emits conntrack zone assignment rules for both
	// directions of DNAT'd flows. Used by prerouting_ctzone and
	// output_ctzone to keep their rule sets in sync.
	writeCTZoneRules := func(b *strings.Builder, forwards []PortForwardVIP) {
		for _, pf := range forwards {
			for _, r := range pf.Rules {
				addrs := r.destAddrs()
				if len(addrs) == 0 {
					continue
				}
				destPort := r.DestPort
				if destPort == 0 {
					destPort = r.Port
				}
				fmt.Fprintf(b, "        ip daddr %s %s dport %d ct zone set %d\n",
					pf.VIP, r.Proto, r.Port, ctZone)
				for _, addr := range addrs {
					fmt.Fprintf(b, "        ip saddr %s %s sport %d ct zone set %d\n",
						addr, r.Proto, destPort, ctZone)
				}
			}
		}
	}

	// Collect all VIP addresses (needed for fwmark chain and SNAT).
	allVIPs := make([]string, 0, len(forwards))
	for _, pf := range forwards {
		allVIPs = append(allVIPs, pf.VIP)
	}

	// Collect per-rule masquerade entries. Each entry describes a backend
	// destination that needs SNAT. Per-rule granularity is essential when
	// a VIP has both local and remote backends: local backends must NOT
	// be masqueraded (the reply originates locally and is handled by the
	// output chains), while remote backends MUST be masqueraded so their
	// replies return to this node for reverse NAT.
	type snatEntry struct {
		addr  string // backend dest address (post-DNAT)
		proto string
		port  int // backend dest port (post-DNAT)
	}
	var snatEntries []snatEntry
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			if !r.effectiveMasquerade(pf.Masquerade) {
				continue
			}
			addrs := r.destAddrs()
			if len(addrs) == 0 {
				continue
			}
			destPort := r.DestPort
			if destPort == 0 {
				destPort = r.Port
			}
			for _, addr := range addrs {
				snatEntries = append(snatEntries, snatEntry{
					addr:  addr,
					proto: r.Proto,
					port:  destPort,
				})
			}
		}
	}

	// Collect VIPs with hairpin_masquerade enabled. Hairpin masquerade
	// applies SNAT only to traffic from provider networks so that the
	// backend always replies through this node — solving the hairpin NAT
	// problem where a FIP on the same node connects to a port-forwarded VIP.
	// Unlike the VIP-level masquerade (which masquerades all traffic), this
	// is source-selective: only packets whose source is within a provider
	// network are masqueraded.
	var hairpinVIPs []string
	for _, pf := range forwards {
		if pf.HairpinMasquerade {
			hairpinVIPs = append(hairpinVIPs, pf.VIP)
		}
	}

	// Collect VIPs with router_masquerade enabled. Router masquerade applies
	// SNAT only to traffic whose source is a known router SNAT IP (discovered
	// from OVN NB NAT entries of type "snat"). This solves the hairpin NAT
	// problem for instances behind a router that have no Floating IP: the
	// router SNAT'd source IP is an OVN-managed address, so without masquerade
	// the backend's reply enters OVN's pipeline directly — bypassing this
	// node's conntrack — and the reverse DNAT never fires.
	// Unlike hairpin_masquerade (which uses the full provider CIDR), this is
	// more surgical: only the specific known SNAT IPs are masqueraded, leaving
	// all other external clients' IPs fully preserved.
	var routerMasqVIPs []string
	for _, pf := range forwards {
		if pf.RouterMasquerade {
			routerMasqVIPs = append(routerMasqVIPs, pf.VIP)
		}
	}

	var b strings.Builder

	fmt.Fprintf(&b, "table ip %s {\n", nftTableName)

	// Chain: assign a shared conntrack zone for DNAT'd flows.
	// Priority raw (-300) runs BEFORE conntrack (-200). This is essential
	// because the original direction enters on a VRF interface (provider)
	// while the reply enters on a non-VRF interface (control-plane).
	// Without a shared zone, conntrack cannot correlate them and the
	// reverse NAT (un-masquerade / un-DNAT) fails silently.
	b.WriteString("    chain prerouting_ctzone {\n")
	b.WriteString("        type filter hook prerouting priority raw; policy accept;\n")
	writeCTZoneRules(&b, forwards)
	b.WriteString("    }\n")

	// Chain: assign conntrack zone for locally generated DNAT traffic.
	// Mirrors prerouting_ctzone for the output hook. This is needed when a
	// DNAT backend runs on the same host: the reply from the local process
	// (e.g. docker-proxy) originates in OUTPUT, not PREROUTING, and
	// conntrack must use the shared zone to find the DNAT entry and apply the
	// reverse translation. Without this, the reply exits with the backend
	// address as source (no reverse DNAT) and is never policy-routed back
	// into the provider VRF.
	b.WriteString("    chain output_ctzone {\n")
	b.WriteString("        type filter hook output priority raw; policy accept;\n")
	writeCTZoneRules(&b, forwards)
	b.WriteString("    }\n")

	// Chain: DNAT rules.
	// For rules with a single backend: direct DNAT.
	// For rules with multiple backends: jhash on source IP for sticky
	// load balancing — the same client IP always maps to the same backend.
	b.WriteString("    chain prerouting_dnat {\n")
	b.WriteString("        type nat hook prerouting priority dstnat; policy accept;\n")
	for _, pf := range forwards {
		for _, r := range pf.Rules {
			destPort := r.DestPort
			if destPort == 0 {
				destPort = r.Port
			}
			addrs := r.destAddrs()
			if len(addrs) == 0 {
				continue
			}
			if len(addrs) == 1 {
				fmt.Fprintf(&b, "        ip daddr %s %s dport %d dnat to %s:%d\n",
					pf.VIP, r.Proto, r.Port, addrs[0], destPort)
			} else {
				// jhash: consistent source-IP hashing distributes clients
				// across backends. Same client IP → same backend (sticky).
				//
				// nft note: inside a verdict map for `dnat to`, the
				// (addr, port) target must use the concat operator
				// (`addr . port`), not the inline `addr:port` form. The
				// inline form is only valid for a single non-mapped
				// dnat target.
				fmt.Fprintf(&b, "        ip daddr %s %s dport %d dnat to jhash ip saddr mod %d map { ",
					pf.VIP, r.Proto, r.Port, len(addrs))
				for i, addr := range addrs {
					if i > 0 {
						b.WriteString(", ")
					}
					fmt.Fprintf(&b, "%d : %s . %d", i, addr, destPort)
				}
				b.WriteString(" }\n")
			}
		}
	}
	b.WriteString("    }\n")

	// Chain: policy-route DNAT'd packets in both directions.
	// Priority filter (0) runs after dstnat (-100) so ct status dnat is
	// visible. Two fwmarks steer traffic into different routing tables:
	//   - Original direction (client→backend): fwmark 0x100 → lookup main
	//     so the packet escapes the VRF and reaches the backend via the
	//     default VRF's routing (e.g. control-plane network).
	//   - Reply direction (backend→client): fwmark 0x200 → lookup VRF
	//     so the response exits via the provider network where the VIP
	//     source address is legitimate (not filtered as spoofed).
	b.WriteString("    chain prerouting_fwmark {\n")
	b.WriteString("        type filter hook prerouting priority filter; policy accept;\n")
	if len(allVIPs) == 1 {
		fmt.Fprintf(&b, "        ct direction original ct status dnat ct original daddr %s meta mark set 0x%x\n",
			allVIPs[0], dnatFwmark)
		fmt.Fprintf(&b, "        ct direction reply ct status dnat ct original daddr %s meta mark set 0x%x\n",
			allVIPs[0], dnatReplyFwmark)
	} else {
		// nft note: for a single bare-IP literal, `ct original daddr`
		// can infer the address family. For an anonymous set with
		// multiple values it cannot, and rejects with "specify either
		// ip or ip6 for address matching". The explicit `ip daddr`
		// pins the family so the set parses.
		vipSet := strings.Join(allVIPs, ", ")
		fmt.Fprintf(&b, "        ct direction original ct status dnat ct original ip daddr { %s } meta mark set 0x%x\n",
			vipSet, dnatFwmark)
		fmt.Fprintf(&b, "        ct direction reply ct status dnat ct original ip daddr { %s } meta mark set 0x%x\n",
			vipSet, dnatReplyFwmark)
	}
	b.WriteString("    }\n")

	// Chain: policy-route locally generated DNAT reply packets.
	// When a DNAT backend runs on the same host, reply traffic originates
	// in OUTPUT (not PREROUTING). This chain mirrors the reply-direction
	// mark from prerouting_fwmark. Uses "type route" so the mark change
	// triggers a routing re-evaluation, steering the reply through the
	// veth pair back into the provider VRF.
	b.WriteString("    chain output_fwmark {\n")
	b.WriteString("        type route hook output priority filter; policy accept;\n")
	if len(allVIPs) == 1 {
		fmt.Fprintf(&b, "        ct direction reply ct status dnat ct original daddr %s meta mark set 0x%x\n",
			allVIPs[0], dnatReplyFwmark)
	} else {
		// Same family-inference issue as prerouting_fwmark — see
		// the comment there. The explicit `ip daddr` pins the family
		// so the multi-element set parses.
		vipSet := strings.Join(allVIPs, ", ")
		fmt.Fprintf(&b, "        ct direction reply ct status dnat ct original ip daddr { %s } meta mark set 0x%x\n",
			vipSet, dnatReplyFwmark)
	}
	b.WriteString("    }\n")

	// Chain: veth forward guard (security)
	// Whitelists legitimate veth-leak return traffic and drops everything
	// else going backwards through the veth pair.
	b.WriteString("    chain forward_veth_guard {\n")
	b.WriteString("        type filter hook forward priority filter; policy accept;\n")
	// Allow existing veth leak return traffic (source in provider networks)
	if len(providerNetworks) > 0 {
		nets := make([]string, len(providerNetworks))
		for i, n := range providerNetworks {
			nets[i] = n.String()
		}
		fmt.Fprintf(&b, "        oifname \"%s\" ip saddr { %s } accept\n",
			vethDefaultName, strings.Join(nets, ", "))
	}
	// Allow DNAT reply traffic returning to the VRF via veth pair.
	fmt.Fprintf(&b, "        oifname \"%s\" meta mark 0x%x accept\n",
		vethDefaultName, dnatReplyFwmark)
	// Drop everything else going backwards through veth
	fmt.Fprintf(&b, "        oifname \"%s\" drop\n", vethDefaultName)
	b.WriteString("    }\n")

	// Chain: clear DNAT reply fwmark before packets cross the veth pair.
	// Without this, the fwmark persists into the provider VRF and the
	// ip rule matches again, creating a routing loop (veth-default →
	// veth-provider → table 201 → veth-default → …).
	b.WriteString("    chain postrouting_fwmark_clear {\n")
	b.WriteString("        type filter hook postrouting priority filter; policy accept;\n")
	fmt.Fprintf(&b, "        oifname \"%s\" meta mark 0x%x meta mark set 0\n",
		vethDefaultName, dnatReplyFwmark)
	b.WriteString("    }\n")

	// Chain: SNAT for three cases:
	//
	// 1. Per-rule masquerade (masquerade: true on VIP or rule): matches on the
	//    post-DNAT destination (backend address). Local backends must NOT be
	//    masqueraded — their replies are handled by the output chains instead.
	//    Uses interface masquerade so replies from remote backends always return
	//    to this node.
	//
	// 2. Hairpin masquerade (hairpin_masquerade: true on VIP): matches on the
	//    pre-DNAT source address being within a provider network. Solves the
	//    hairpin NAT problem: when a FIP in the provider network connects to a
	//    VIP on the same node, the backend would otherwise see the FIP as source
	//    and may route the reply asymmetrically (not back through this node).
	//    By masquerading only provider-sourced traffic, non-hairpin clients are
	//    unaffected.
	//
	// 3. Router masquerade (router_masquerade: true on VIP): matches on the
	//    pre-DNAT source address being a known router SNAT IP. Solves the
	//    hairpin NAT problem for instances without a FIP that reach the VIP
	//    through a router: the router's external IP is OVN-managed, so the
	//    backend's reply would normally enter OVN's pipeline directly (bypassing
	//    this node's conntrack), preventing the reverse DNAT from firing.
	//    More surgical than hairpin_masquerade: only specific SNAT IPs are
	//    masqueraded, leaving external clients' IPs fully preserved.
	hairpinNeeded := len(hairpinVIPs) > 0 && len(providerNetworks) > 0
	routerMasqNeeded := len(routerMasqVIPs) > 0 && len(snatIPs) > 0
	if len(snatEntries) > 0 || hairpinNeeded || routerMasqNeeded {
		b.WriteString("    chain postrouting_snat {\n")
		b.WriteString("        type nat hook postrouting priority srcnat; policy accept;\n")
		for _, e := range snatEntries {
			fmt.Fprintf(&b, "        ip daddr %s %s dport %d ct status dnat masquerade\n",
				e.addr, e.proto, e.port)
		}
		if hairpinNeeded {
			nets := make([]string, len(providerNetworks))
			for i, n := range providerNetworks {
				nets[i] = n.String()
			}
			var srcMatch string
			if len(providerNetworks) == 1 {
				srcMatch = nets[0]
			} else {
				srcMatch = "{ " + strings.Join(nets, ", ") + " }"
			}
			for _, vip := range hairpinVIPs {
				fmt.Fprintf(&b, "        ip saddr %s ct original daddr %s ct status dnat masquerade\n",
					srcMatch, vip)
			}
		}
		if routerMasqNeeded {
			sortedSNATIPs := append([]string{}, snatIPs...)
			sort.Strings(sortedSNATIPs)
			sortedSNATIPs = slices.Compact(sortedSNATIPs)
			var srcMatch string
			if len(sortedSNATIPs) == 1 {
				srcMatch = sortedSNATIPs[0]
			} else {
				srcMatch = "{ " + strings.Join(sortedSNATIPs, ", ") + " }"
			}
			for _, vip := range routerMasqVIPs {
				fmt.Fprintf(&b, "        ip saddr %s ct original daddr %s ct status dnat masquerade\n",
					srcMatch, vip)
			}
		}
		b.WriteString("    }\n")
	}

	b.WriteString("}\n")
	return b.String()
}
