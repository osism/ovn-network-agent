package main

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

// dnatCTZoneDefault is the conntrack zone the tests pass to buildNftRuleset. It
// mirrors the production default (Config.PortForwardCTZone), which the tests
// never vary; it lives here rather than in nftables.go so production code
// carries no unused constant.
const dnatCTZoneDefault = 64000

func TestBuildNftRuleset(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
				{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"},
				{Proto: "tcp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
				{Proto: "udp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
			},
		},
	}
	_, provNet, _ := net.ParseCIDR("198.51.100.0/24")

	result := buildNftRuleset(forwards, []*net.IPNet{provNet}, nil, dnatCTZoneDefault)

	// Verify table structure
	if !strings.Contains(result, "table ip ovn-network-agent {") {
		t.Error("missing table declaration")
	}

	// Verify conntrack zone chains (prerouting + output)
	if !strings.Contains(result, "chain prerouting_ctzone") {
		t.Error("missing prerouting_ctzone chain")
	}
	if !strings.Contains(result, "type filter hook prerouting priority raw") {
		t.Error("ctzone chain must be raw priority (before conntrack)")
	}
	// Verify output_ctzone chain exists and has correct rules
	outputCtzone := extractChain(result, "output_ctzone")
	if outputCtzone == "" {
		t.Fatal("missing output_ctzone chain (needed for same-host backends)")
	}
	if !strings.Contains(outputCtzone, "type filter hook output priority raw") {
		t.Error("output ctzone chain must be raw priority")
	}
	if !strings.Contains(outputCtzone, "ip daddr 198.51.100.10 tcp dport 80 ct zone set 64000") {
		t.Error("output_ctzone missing ctzone rule for original direction (VIP:80)")
	}
	if !strings.Contains(outputCtzone, "ip saddr 10.0.0.100 tcp sport 80 ct zone set 64000") {
		t.Error("output_ctzone missing ctzone rule for reply direction (backend:80)")
	}

	// Also verify prerouting_ctzone has the same rules
	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 80 ct zone set 64000") {
		t.Error("missing ctzone rule for original direction (VIP:80)")
	}
	if !strings.Contains(result, "ip saddr 10.0.0.100 tcp sport 80 ct zone set 64000") {
		t.Error("missing ctzone rule for reply direction (backend:80)")
	}

	// Verify DNAT rules
	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 80 dnat to 10.0.0.100:80") {
		t.Error("missing DNAT rule for port 80")
	}
	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing DNAT rule for port 53 with port translation")
	}

	// Verify fwmark chains mark DNAT'd packets (both directions)
	if !strings.Contains(result, "chain prerouting_fwmark") {
		t.Error("missing prerouting_fwmark chain")
	}
	if !strings.Contains(result, "ct direction original ct status dnat ct original daddr 198.51.100.10 meta mark set 0x100") {
		t.Error("missing forward fwmark rule for DNAT'd packets")
	}
	if !strings.Contains(result, "ct direction reply ct status dnat ct original daddr 198.51.100.10 meta mark set 0x200") {
		t.Error("missing reply fwmark rule for DNAT'd return packets")
	}

	// Verify output_fwmark chain for same-host backend replies
	if !strings.Contains(result, "chain output_fwmark") {
		t.Error("missing output_fwmark chain (needed for same-host backends)")
	}
	if !strings.Contains(result, "type route hook output priority filter") {
		t.Error("output fwmark chain must be type route (triggers re-routing)")
	}

	// Verify guard chain allows provider network
	if !strings.Contains(result, "ip saddr { 198.51.100.0/24 } accept") {
		t.Error("missing provider network allow rule in guard chain")
	}

	// Verify guard chain allows DNAT reply traffic
	if !strings.Contains(result, `oifname "veth-default" meta mark 0x200 accept`) {
		t.Error("missing DNAT reply accept rule in guard chain")
	}

	// Verify guard chain drops unauthorized traffic
	if !strings.Contains(result, `oifname "veth-default" drop`) {
		t.Error("missing drop rule in guard chain")
	}
}

func TestBuildNftRulesetMultipleVIPs(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
			},
		},
		{
			VIP: "198.51.100.20",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"},
				{Proto: "udp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 80") {
		t.Error("missing DNAT rule for first VIP")
	}
	if !strings.Contains(result, "ip daddr 198.51.100.20 tcp dport 443") {
		t.Error("missing DNAT rule for second VIP")
	}
	if !strings.Contains(result, "ip daddr 198.51.100.20 udp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing DNAT rule for second VIP port 53")
	}

	// Verify fwmark chain uses set syntax for multiple VIPs (both directions)
	if !strings.Contains(result, "ct direction original ct status dnat ct original ip daddr { 198.51.100.10, 198.51.100.20 } meta mark set 0x100") {
		t.Errorf("forward fwmark should use set syntax for multiple VIPs, got:\n%s", result)
	}
	if !strings.Contains(result, "ct direction reply ct status dnat ct original ip daddr { 198.51.100.10, 198.51.100.20 } meta mark set 0x200") {
		t.Errorf("reply fwmark should use set syntax for multiple VIPs, got:\n%s", result)
	}

	// Verify output_fwmark uses set syntax for multiple VIPs
	if !strings.Contains(result, "chain output_fwmark") {
		t.Error("missing output_fwmark chain")
	}
	// Extract output_fwmark chain content and verify set syntax
	outputFwmark := extractChain(result, "output_fwmark")
	if !strings.Contains(outputFwmark, "ct direction reply ct status dnat ct original ip daddr { 198.51.100.10, 198.51.100.20 } meta mark set 0x200") {
		t.Errorf("output_fwmark should use set syntax for multiple VIPs, got:\n%s", outputFwmark)
	}

	// Verify output_ctzone has rules for both VIPs
	outputCtzone := extractChain(result, "output_ctzone")
	if outputCtzone == "" {
		t.Fatal("missing output_ctzone chain")
	}
	if !strings.Contains(outputCtzone, "ip daddr 198.51.100.10") {
		t.Error("output_ctzone missing rules for first VIP")
	}
	if !strings.Contains(outputCtzone, "ip daddr 198.51.100.20") {
		t.Error("output_ctzone missing rules for second VIP")
	}
}

func TestBuildNftRulesetNoProviderNetworks(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	// Guard chain should not have provider network allow rule
	if strings.Contains(result, "ip saddr {") {
		t.Error("should not have provider network accept rule when no provider networks given")
	}

	// Should still have the drop rule
	if !strings.Contains(result, `oifname "veth-default" drop`) {
		t.Error("missing drop rule in guard chain")
	}
}

func TestBuildNftRulesetDestPortDefault(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100", DestPort: 0},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	// dest_port=0 should use port as default
	if !strings.Contains(result, "dnat to 10.0.0.100:80") {
		t.Error("dest_port=0 should default to port value")
	}
}

func TestBuildNftRulesetSamePortDifferentProto(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
				{Proto: "udp", Port: 53, DestAddr: "10.0.0.200", DestPort: 1053},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	if !strings.Contains(result, "tcp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing TCP DNAT rule for port 53")
	}
	if !strings.Contains(result, "udp dport 53 dnat to 10.0.0.200:1053") {
		t.Error("missing UDP DNAT rule for port 53")
	}
}

func TestBuildNftRulesetMultipleProviderNetworks(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP:   "198.51.100.10",
			Rules: []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"}},
		},
	}
	_, net1, _ := net.ParseCIDR("198.51.100.0/24")
	_, net2, _ := net.ParseCIDR("203.0.113.0/24")

	result := buildNftRuleset(forwards, []*net.IPNet{net1, net2}, nil, dnatCTZoneDefault)

	if !strings.Contains(result, "198.51.100.0/24, 203.0.113.0/24") {
		t.Errorf("guard chain should contain both provider networks, got:\n%s", result)
	}
}

func TestBuildNftRulesetMaxPort(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP:   "198.51.100.10",
			Rules: []PortForwardRule{{Proto: "tcp", Port: 65535, DestAddr: "10.0.0.100", DestPort: 65535}},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	if !strings.Contains(result, "tcp dport 65535 dnat to 10.0.0.100:65535") {
		t.Error("missing rule for max port 65535")
	}
}

func TestBuildNftRulesetMasquerade(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP:        "198.51.100.10",
			Masquerade: true,
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"},
			},
		},
		{
			VIP: "198.51.100.20",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.200"},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	// Should have postrouting SNAT chain (VIP-level masquerade inherits to rules)
	if !strings.Contains(result, "chain postrouting_snat") {
		t.Fatal("missing postrouting_snat chain")
	}
	if !strings.Contains(result, "type nat hook postrouting priority srcnat") {
		t.Error("missing srcnat hook in SNAT chain")
	}
	// Per-backend masquerade rule for the masquerade-enabled VIP's backend
	if !strings.Contains(result, "ip daddr 10.0.0.100 tcp dport 443 ct status dnat masquerade") {
		t.Errorf("missing per-backend masquerade rule, got:\n%s", result)
	}
	// Non-masquerade VIP's backend should NOT have a masquerade rule
	if strings.Contains(result, "10.0.0.200") && strings.Contains(result, "masquerade") {
		snat := extractChain(result, "postrouting_snat")
		if strings.Contains(snat, "10.0.0.200") {
			t.Error("non-masquerade VIP's backend should not have masquerade rule")
		}
	}
}

func TestBuildNftRulesetPerRuleMasquerade(t *testing.T) {
	// Mirrors the real-world scenario: one VIP with both local and remote
	// backends. Local backends (DNS → same host) must NOT be masqueraded,
	// remote backends (HTTP → different host) MUST be masqueraded.
	masqTrue := true
	masqFalse := false
	forwards := []PortForwardVIP{
		{
			VIP:        "194.93.78.239",
			ManageVIP:  true,
			Masquerade: true, // VIP-level default
			Rules: []PortForwardRule{
				// Local backend: override masquerade to false
				{Proto: "udp", Port: 53, DestAddr: "10.1.8.32", DestPort: 1053, Masquerade: &masqFalse},
				{Proto: "tcp", Port: 53, DestAddr: "10.1.8.32", DestPort: 1053, Masquerade: &masqFalse},
				// Remote backends: inherit VIP masquerade (true)
				{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"},
				{Proto: "tcp", Port: 443, DestAddr: "10.1.8.226"},
				// Explicit true also works
				{Proto: "tcp", Port: 8080, DestAddr: "10.1.8.227", Masquerade: &masqTrue},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)
	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain")
	}

	// Remote backends should have masquerade rules
	if !strings.Contains(snat, "ip daddr 10.1.8.226 tcp dport 80 ct status dnat masquerade") {
		t.Errorf("missing masquerade for remote backend :80, got:\n%s", snat)
	}
	if !strings.Contains(snat, "ip daddr 10.1.8.226 tcp dport 443 ct status dnat masquerade") {
		t.Errorf("missing masquerade for remote backend :443, got:\n%s", snat)
	}
	if !strings.Contains(snat, "ip daddr 10.1.8.227 tcp dport 8080 ct status dnat masquerade") {
		t.Errorf("missing masquerade for explicit-true backend, got:\n%s", snat)
	}

	// Local backends must NOT have masquerade rules
	if strings.Contains(snat, "10.1.8.32") {
		t.Errorf("local backend 10.1.8.32 must NOT be masqueraded, got:\n%s", snat)
	}

	// Verify DNAT rules exist for ALL backends (local and remote)
	if !strings.Contains(result, "dnat to 10.1.8.32:1053") {
		t.Error("missing DNAT rule for local backend")
	}
	if !strings.Contains(result, "dnat to 10.1.8.226:80") {
		t.Error("missing DNAT rule for remote backend :80")
	}

	// Verify output chains exist (needed for local backend)
	if !strings.Contains(result, "chain output_ctzone") {
		t.Error("missing output_ctzone chain")
	}
	if !strings.Contains(result, "chain output_fwmark") {
		t.Error("missing output_fwmark chain")
	}
}

func TestBuildNftRulesetNoMasquerade(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	// Should NOT have postrouting SNAT chain
	if strings.Contains(result, "postrouting_snat") {
		t.Error("should not have SNAT chain when no VIP has masquerade enabled")
	}
}

func TestBuildNftRulesetMultiBackend(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "udp", Port: 53, DestAddrs: []string{"10.0.0.200", "10.0.0.201", "10.0.0.202"}, DestPort: 1053},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	// Verify jhash-based DNAT rule. nft requires the concat operator
	// (`addr . port`) inside a verdict map; the inline `addr:port` form
	// is only valid for non-mapped dnat targets.
	expected := "ip daddr 198.51.100.10 udp dport 53 dnat to jhash ip saddr mod 3 map { 0 : 10.0.0.200 . 1053, 1 : 10.0.0.201 . 1053, 2 : 10.0.0.202 . 1053 }"
	if !strings.Contains(result, expected) {
		t.Errorf("missing jhash DNAT rule.\nwant: %s\ngot:\n%s", expected, result)
	}

	// Verify conntrack zone entries for ALL backends
	for _, addr := range []string{"10.0.0.200", "10.0.0.201", "10.0.0.202"} {
		ct := fmt.Sprintf("ip saddr %s udp sport 1053 ct zone set 64000", addr)
		if !strings.Contains(result, ct) {
			t.Errorf("missing ctzone rule for backend %s", addr)
		}
	}
}

func TestBuildNftRulesetMultiBackendTwoBackends(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 443, DestAddrs: []string{"10.0.0.100", "10.0.0.101"}},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	// With two backends, jhash mod 2. Map values use the concat operator
	// (`addr . port`); see TestBuildNftRulesetMultiBackend for context.
	expected := "ip daddr 198.51.100.10 tcp dport 443 dnat to jhash ip saddr mod 2 map { 0 : 10.0.0.100 . 443, 1 : 10.0.0.101 . 443 }"
	if !strings.Contains(result, expected) {
		t.Errorf("missing jhash DNAT rule for 2 backends.\nwant: %s\ngot:\n%s", expected, result)
	}
}

func TestBuildNftRulesetSingleElementDestAddrs(t *testing.T) {
	// A dest_addrs list with one entry should produce direct DNAT, not jhash.
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddrs: []string{"10.0.0.100"}},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 80 dnat to 10.0.0.100:80") {
		t.Error("single-element dest_addrs should produce direct DNAT")
	}
	if strings.Contains(result, "jhash") {
		t.Error("single-element dest_addrs should not use jhash")
	}
}

func TestBuildNftRulesetMixedSingleAndMultiBackend(t *testing.T) {
	forwards := []PortForwardVIP{
		{
			VIP: "198.51.100.10",
			Rules: []PortForwardRule{
				// Single backend: should use direct DNAT
				{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"},
				// Multi backend: should use jhash
				{Proto: "udp", Port: 53, DestAddrs: []string{"10.0.0.200", "10.0.0.201"}, DestPort: 1053},
			},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	// Single backend → direct DNAT
	if !strings.Contains(result, "ip daddr 198.51.100.10 tcp dport 443 dnat to 10.0.0.100:443") {
		t.Error("single-backend rule should use direct DNAT, not jhash")
	}

	// Multi backend → jhash
	if !strings.Contains(result, "jhash ip saddr mod 2 map") {
		t.Error("multi-backend rule should use jhash")
	}
}

func TestBuildNftRulesetHairpinMasquerade(t *testing.T) {
	_, provNet, _ := net.ParseCIDR("5.182.234.0/24")
	forwards := []PortForwardVIP{
		{
			VIP:               "194.93.78.239",
			ManageVIP:         true,
			HairpinMasquerade: true,
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"},
				{Proto: "tcp", Port: 443, DestAddr: "10.1.8.226"},
			},
		},
	}

	result := buildNftRuleset(forwards, []*net.IPNet{provNet}, nil, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain (needed for hairpin masquerade)")
	}

	// Hairpin masquerade rule: source from provider net, original dest is VIP
	expected := "ip saddr 5.182.234.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade"
	if !strings.Contains(snat, expected) {
		t.Errorf("missing hairpin masquerade rule.\nwant: %s\ngot:\n%s", expected, snat)
	}

	// Must NOT masquerade all traffic (no per-backend rules since masquerade: false)
	if strings.Contains(snat, "ip daddr 10.1.8.226") {
		t.Errorf("hairpin_masquerade must not emit per-backend rules, got:\n%s", snat)
	}
}

func TestBuildNftRulesetHairpinMasqueradeMultipleProviderNets(t *testing.T) {
	_, net1, _ := net.ParseCIDR("5.182.234.0/24")
	_, net2, _ := net.ParseCIDR("203.0.113.0/24")
	forwards := []PortForwardVIP{
		{
			VIP:               "194.93.78.239",
			HairpinMasquerade: true,
			Rules:             []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
	}

	result := buildNftRuleset(forwards, []*net.IPNet{net1, net2}, nil, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain")
	}

	expected := "ip saddr { 5.182.234.0/24, 203.0.113.0/24 } ct original daddr 194.93.78.239 ct status dnat masquerade"
	if !strings.Contains(snat, expected) {
		t.Errorf("missing hairpin masquerade rule with set syntax.\nwant: %s\ngot:\n%s", expected, snat)
	}
}

func TestBuildNftRulesetHairpinMasqueradeWithoutProviderNets(t *testing.T) {
	// hairpin_masquerade: true but no provider networks known yet (initial setup).
	// The SNAT chain must NOT be emitted to avoid matching all traffic.
	forwards := []PortForwardVIP{
		{
			VIP:               "194.93.78.239",
			HairpinMasquerade: true,
			Rules:             []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	if strings.Contains(result, "postrouting_snat") {
		t.Error("postrouting_snat must not be emitted when provider networks are unknown")
	}
}

func TestBuildNftRulesetHairpinAndPerRuleMasqueradeCombined(t *testing.T) {
	// VIP with both hairpin_masquerade and per-rule masquerade for a remote backend.
	// Both rules should coexist in postrouting_snat.
	masqTrue := true
	_, provNet, _ := net.ParseCIDR("5.182.234.0/24")
	forwards := []PortForwardVIP{
		{
			VIP:               "194.93.78.239",
			HairpinMasquerade: true,
			Rules: []PortForwardRule{
				{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226", Masquerade: &masqTrue},
			},
		},
	}

	result := buildNftRuleset(forwards, []*net.IPNet{provNet}, nil, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain")
	}

	// Per-backend masquerade rule (masquerade: true on rule)
	if !strings.Contains(snat, "ip daddr 10.1.8.226 tcp dport 80 ct status dnat masquerade") {
		t.Errorf("missing per-backend masquerade rule, got:\n%s", snat)
	}

	// Hairpin masquerade rule (hairpin_masquerade: true on VIP)
	if !strings.Contains(snat, "ip saddr 5.182.234.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade") {
		t.Errorf("missing hairpin masquerade rule, got:\n%s", snat)
	}
}

func TestBuildNftRulesetRouterMasquerade(t *testing.T) {
	// Instance behind a router (no FIP) connects to a VIP on the same node.
	// The router SNAT'd source IP is an OVN-managed address; without masquerade
	// the backend's reply enters OVN's pipeline and the reverse DNAT never fires.
	// router_masquerade generates a SNAT rule targeted at the specific SNAT IPs.
	snatIPs := []string{"203.0.113.50"}
	forwards := []PortForwardVIP{
		{
			VIP:              "194.93.78.239",
			RouterMasquerade: true,
			Rules:            []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
	}

	result := buildNftRuleset(forwards, nil, snatIPs, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain (needed for router masquerade)")
	}

	expected := "ip saddr 203.0.113.50 ct original daddr 194.93.78.239 ct status dnat masquerade"
	if !strings.Contains(snat, expected) {
		t.Errorf("missing router masquerade rule.\nwant: %s\ngot:\n%s", expected, snat)
	}

	// Must NOT masquerade all traffic (no per-backend rules since masquerade: false)
	if strings.Contains(snat, "ip daddr 10.1.8.226") {
		t.Errorf("router_masquerade must not emit per-backend rules, got:\n%s", snat)
	}
}

func TestBuildNftRulesetRouterMasqueradeMultipleSNATIPs(t *testing.T) {
	// Multiple router SNAT IPs: nftables set syntax { ip1, ip2 }.
	// IPs must be sorted for deterministic output.
	snatIPs := []string{"203.0.113.51", "203.0.113.50"} // intentionally unsorted
	forwards := []PortForwardVIP{
		{
			VIP:              "194.93.78.239",
			RouterMasquerade: true,
			Rules:            []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
	}

	result := buildNftRuleset(forwards, nil, snatIPs, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain")
	}

	expected := "ip saddr { 203.0.113.50, 203.0.113.51 } ct original daddr 194.93.78.239 ct status dnat masquerade"
	if !strings.Contains(snat, expected) {
		t.Errorf("missing router masquerade rule with set syntax.\nwant: %s\ngot:\n%s", expected, snat)
	}
}

func TestBuildNftRulesetRouterMasqueradeDuplicateSNATIPs(t *testing.T) {
	// OVN may return the same external IP for multiple NAT entries (e.g. two
	// routers sharing a SNAT address). Duplicates must be removed before
	// building the nftables set — { ip, ip } is invalid syntax.
	snatIPs := []string{"203.0.113.50", "203.0.113.50"} // intentional duplicate
	forwards := []PortForwardVIP{
		{
			VIP:              "194.93.78.239",
			RouterMasquerade: true,
			Rules:            []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
	}

	result := buildNftRuleset(forwards, nil, snatIPs, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain")
	}

	// Deduplicated: single IP, no set syntax.
	expected := "ip saddr 203.0.113.50 ct original daddr 194.93.78.239 ct status dnat masquerade"
	if !strings.Contains(snat, expected) {
		t.Errorf("duplicate SNAT IPs must be deduplicated to single-IP form.\nwant: %s\ngot:\n%s", expected, snat)
	}

	// Set syntax must not appear (would be invalid nftables).
	if strings.Contains(snat, "{ 203.0.113.50, 203.0.113.50 }") {
		t.Error("duplicate SNAT IPs produced invalid nftables set syntax")
	}
}

func TestBuildNftRulesetRouterMasqueradeWithoutSNATIPs(t *testing.T) {
	// router_masquerade: true but no SNAT IPs known yet (no routers discovered).
	// The SNAT chain must NOT be emitted to avoid accidental masquerade.
	forwards := []PortForwardVIP{
		{
			VIP:              "194.93.78.239",
			RouterMasquerade: true,
			Rules:            []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
	}

	result := buildNftRuleset(forwards, nil, nil, dnatCTZoneDefault)

	if strings.Contains(result, "postrouting_snat") {
		t.Error("postrouting_snat must not be emitted when no SNAT IPs are known")
	}
}

func TestBuildNftRulesetRouterAndHairpinMasqueradeCombined(t *testing.T) {
	// VIP with both router_masquerade and hairpin_masquerade.
	// Both rules should coexist in postrouting_snat.
	_, provNet, _ := net.ParseCIDR("203.0.113.0/24")
	snatIPs := []string{"203.0.113.50"}
	forwards := []PortForwardVIP{
		{
			VIP:               "194.93.78.239",
			HairpinMasquerade: true,
			RouterMasquerade:  true,
			Rules:             []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
	}

	result := buildNftRuleset(forwards, []*net.IPNet{provNet}, snatIPs, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain")
	}

	// Hairpin masquerade rule (CIDR-based)
	if !strings.Contains(snat, "ip saddr 203.0.113.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade") {
		t.Errorf("missing hairpin masquerade rule, got:\n%s", snat)
	}

	// Router masquerade rule (IP-based)
	if !strings.Contains(snat, "ip saddr 203.0.113.50 ct original daddr 194.93.78.239 ct status dnat masquerade") {
		t.Errorf("missing router masquerade rule, got:\n%s", snat)
	}
}

func TestBuildNftRulesetRouterMasqueradeDoesNotAffectOtherVIPs(t *testing.T) {
	// router_masquerade on one VIP must not generate rules for other VIPs.
	snatIPs := []string{"203.0.113.50"}
	forwards := []PortForwardVIP{
		{
			VIP:              "194.93.78.239",
			RouterMasquerade: true,
			Rules:            []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.226"}},
		},
		{
			VIP:   "194.93.78.240",
			Rules: []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.1.8.227"}},
		},
	}

	result := buildNftRuleset(forwards, nil, snatIPs, dnatCTZoneDefault)

	snat := extractChain(result, "postrouting_snat")
	if snat == "" {
		t.Fatal("missing postrouting_snat chain")
	}

	// Rule for the router_masquerade VIP
	if !strings.Contains(snat, "ct original daddr 194.93.78.239 ct status dnat masquerade") {
		t.Errorf("missing router masquerade rule for VIP .239, got:\n%s", snat)
	}

	// No rule for the other VIP
	if strings.Contains(snat, "ct original daddr 194.93.78.240") {
		t.Errorf("router masquerade rule must not be emitted for VIP .240 (no router_masquerade), got:\n%s", snat)
	}
}

// extractChain returns the text between "chain <name> {" and its matching "}".
// Handles nested braces from nftables set syntax (e.g. "{ VIP1, VIP2 }").
// Returns empty string if the chain is not found.
func extractChain(ruleset, name string) string {
	marker := "chain " + name + " {"
	start := strings.Index(ruleset, marker)
	if start == -1 {
		return ""
	}
	rest := ruleset[start+len(marker):]
	depth := 1
	for i, ch := range rest {
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i]
			}
		}
	}
	return ""
}
