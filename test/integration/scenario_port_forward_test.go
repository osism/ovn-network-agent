//go:build integration

package integration

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #43 — port-forward (DNAT, conntrack zones,
// hairpin masquerade) integration tests. Each scenario configures the agent
// with a specific port_forwards block, asserts the resulting nft ruleset and
// kernel state via JSON-parsed `nft -j list ruleset` (not regex on text), and
// relies on the harness's scrubLocalState to delete leaked state between runs.
//
// The agent's port-forward feature requires veth_leak_enabled=true (handled
// by the harness defaults) and a loopback1 device in vrf-provider (provisioned
// by setup.sh — EnsureLoopback1 short-circuits the test if it's missing).

const (
	pfTable = testenv.DefaultNftTable
)

// boolPtr returns a pointer to b — used for *bool fields in fixtures.
func boolPtr(b bool) *bool { return &b }

// intPtr returns a pointer to i — used for *int fields in AgentConfig.
func intPtr(i int) *int { return &i }

// pfDefaults returns a Defaults() AgentConfig augmented with the port-forward
// fields tests need. Caller appends to PortForwards.
func pfDefaults(t *testing.T) testenv.AgentConfig {
	t.Helper()
	cfg := testenv.Defaults()
	cfg.PortForwardDev = testenv.PortForwardLoopbackDev
	// Use a non-default ct zone so collisions with anything OVN might use
	// surface as test failures rather than silent crosstalk.
	cfg.PortForwardCTZone = intPtr(64000)
	return cfg
}

// startPFScenario is the port-forward analogue of startScenario. It performs
// the same OVN bring-up plus EnsureLoopback1, so scenarios that don't drive
// any NB/SB state still have the device they need. It also issues a defensive
// scrub of port-forward residue at the start so a previous failed test cannot
// pollute the current one (the registered Cleanup also scrubs at the end via
// Teardown).
func startPFScenario(t *testing.T) testenv.AgentConfig {
	t.Helper()
	testenv.Setup(t)
	testenv.EnsureLoopback1(t)
	// We don't need ovn-northd paused for port-forward scenarios that don't
	// inject SB Port_Bindings, but we do need a clean slate.
	testenv.PauseOVNNorthd(t)
	testenv.PauseOVNController(t)
	// Defensive scrub: drop any nft table / loopback addresses / fwmark
	// rules left behind by a prior failing scenario before we bring the
	// agent up. The Setup-registered Teardown handles the trailing edge.
	testenv.ScrubPortForwardResidue(t)

	// On failure, dump host-side state for postmortem diagnosis. PF
	// scenarios that also call startScenario already register the OVN
	// dump via that helper; calling RegisterFailureDump here covers PF
	// scenarios that drive only nft/loopback state without OVN clients.
	testenv.RegisterFailureDump(t)

	return pfDefaults(t)
}

// TestScenario_PortForwardMatrix (#61 scenario 6).
//
// Table-driven matrix that subsumes the structural single-backend PF
// scenarios. Each row is its own sub-test and gets a fresh startPFScenario,
// so the rows run in isolation just like the previous standalone tests did.
//
// Rows replaced (all asserts preserved verbatim):
//
//   - tcp_single_backend        ← #43 scenario 1
//   - udp_single_backend        ← #61 scenario 1
//   - tcp_port_translation      ← #61 scenario 2
//   - manage_vip_on_off         ← #43 scenario 3
//   - per_rule_masquerade       ← #43 scenario 4
//   - multi_vip_separation      ← #61 scenario 5
//
// Targeted scenarios stay as their own functions where the assertion shape
// does not fit the row template: sticky hashing (jhash map structure),
// hairpin masquerade (provider-CIDR discovery via OVN), L3mdev sysctls
// (global sysctl save/restore), output chains (chain-type assertion),
// SIGTERM cleanup (process lifecycle), and the fwmark policy-routes test
// (kernel ip-rule/route assertions).
func TestScenario_PortForwardMatrix(t *testing.T) {
	type row struct {
		name  string
		setup func(t *testing.T, cfg *testenv.AgentConfig)
		check func(t *testing.T, cfg testenv.AgentConfig)
	}

	rows := []row{
		// tcp_single_backend (#43 scenario 1).
		//
		// A VIP with a single rule and one backend should produce:
		//   - prerouting_dnat:    ip daddr <VIP> tcp dport 80 dnat to <backend>:80
		//   - prerouting_ctzone:  ip daddr <VIP> tcp dport 80 ct zone set <zone>
		//     ip saddr <backend> tcp sport 80 ct zone set <zone>
		//   - prerouting_fwmark:  meta mark set 0x100 / 0x200 (for original/reply)
		{
			name: "tcp_single_backend",
			setup: func(t *testing.T, cfg *testenv.AgentConfig) {
				cfg.PortForwards = []testenv.PortForwardVIPFixture{{
					VIP: "198.51.100.10",
					Rules: []testenv.PortForwardRuleFixture{{
						Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
					}},
				}}
			},
			check: func(t *testing.T, cfg testenv.AgentConfig) {
				const (
					vip     = "198.51.100.10"
					backend = "10.0.0.100"
				)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", vip) &&
							r.HasMatch("tcp", "dport", 80) &&
							r.HasDNATTo(backend, 80)
					},
					15*time.Second, "single-backend DNAT rule for "+vip+":80")
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", vip) &&
							r.HasMatch("tcp", "dport", 80) &&
							r.HasCTZoneSet(*cfg.PortForwardCTZone)
					},
					10*time.Second, "ctzone original-direction rule for "+vip)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "saddr", backend) &&
							r.HasMatch("tcp", "sport", 80) &&
							r.HasCTZoneSet(*cfg.PortForwardCTZone)
					},
					10*time.Second, "ctzone reply-direction rule for backend "+backend)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_fwmark",
					func(r testenv.NftRule) bool { return r.HasMarkSet(testenv.DefaultDnatFwmark) },
					10*time.Second, "prerouting_fwmark sets original-direction mark")
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_fwmark",
					func(r testenv.NftRule) bool { return r.HasMarkSet(testenv.DefaultDnatReplyFwmark) },
					10*time.Second, "prerouting_fwmark sets reply-direction mark")
			},
		},
		// udp_single_backend (#61 scenario 1).
		//
		// Mirror of tcp_single_backend but for proto: udp on port 53.
		// Pins the DNAT statement and the conntrack-zone rules in both
		// directions so a regression that drops `udp` from the protocol
		// switch in the rule builder cannot pass.
		{
			name: "udp_single_backend",
			setup: func(t *testing.T, cfg *testenv.AgentConfig) {
				cfg.PortForwards = []testenv.PortForwardVIPFixture{{
					VIP: "198.51.100.12",
					Rules: []testenv.PortForwardRuleFixture{{
						Proto: "udp", Port: 53, DestAddr: "10.0.0.110",
					}},
				}}
			},
			check: func(t *testing.T, cfg testenv.AgentConfig) {
				const (
					vip     = "198.51.100.12"
					backend = "10.0.0.110"
				)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", vip) &&
							r.HasMatch("udp", "dport", 53) &&
							r.HasDNATTo(backend, 53)
					},
					15*time.Second, "single-backend UDP DNAT rule for "+vip+":53")
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", vip) &&
							r.HasMatch("udp", "dport", 53) &&
							r.HasCTZoneSet(*cfg.PortForwardCTZone)
					},
					10*time.Second, "ctzone original-direction UDP rule for "+vip)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "saddr", backend) &&
							r.HasMatch("udp", "sport", 53) &&
							r.HasCTZoneSet(*cfg.PortForwardCTZone)
					},
					10*time.Second, "ctzone reply-direction UDP rule for backend "+backend)
			},
		},
		// tcp_port_translation (#61 scenario 2).
		//
		// A rule with port=80 and dest_port=8080 should DNAT the listening
		// port (80) to the backend port (8080). dest_port has lived in the
		// fixture schema since day one but no test asserted the translation
		// actually happens — a typo emitting `dnat to <backend>:80` would
		// have slipped through.
		{
			name: "tcp_port_translation",
			setup: func(t *testing.T, cfg *testenv.AgentConfig) {
				cfg.PortForwards = []testenv.PortForwardVIPFixture{{
					VIP: "198.51.100.13",
					Rules: []testenv.PortForwardRuleFixture{{
						Proto: "tcp", Port: 80, DestAddr: "10.0.0.120", DestPort: 8080,
					}},
				}}
			},
			check: func(t *testing.T, cfg testenv.AgentConfig) {
				const (
					vip     = "198.51.100.13"
					backend = "10.0.0.120"
				)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", vip) &&
							r.HasMatch("tcp", "dport", 80) &&
							r.HasDNATTo(backend, 8080)
					},
					15*time.Second, "DNAT must translate port 80 → "+backend+":8080")
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "saddr", backend) &&
							r.HasMatch("tcp", "sport", 8080) &&
							r.HasCTZoneSet(*cfg.PortForwardCTZone)
					},
					10*time.Second, "reply-zone rule must use translated backend port 8080")
			},
		},
		// manage_vip_on_off (#43 scenario 3).
		//
		// manage_vip:true should add the VIP/32 to loopback1; manage_vip:false
		// (the default) should leave it absent. Verified via
		// `ip -j addr show dev loopback1`.
		//
		// Runs in port-forward-only mode (both OVN remotes cleared, the config
		// writer omits empty keys): pf-only is where "manage_vip adds the
		// address" holds unconditionally, because the VIP is always announceable.
		// Routerless full mode would now withhold the address as dormant (#223).
		{
			name: "manage_vip_on_off",
			setup: func(t *testing.T, cfg *testenv.AgentConfig) {
				cfg.OVNNBRemote = ""
				cfg.OVNSBRemote = ""
				cfg.PortForwards = []testenv.PortForwardVIPFixture{
					{
						VIP:       "198.51.100.10",
						ManageVIP: true,
						Rules: []testenv.PortForwardRuleFixture{{
							Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
						}},
					},
					{
						VIP:       "198.51.100.11",
						ManageVIP: false,
						Rules: []testenv.PortForwardRuleFixture{{
							Proto: "tcp", Port: 81, DestAddr: "10.0.0.101",
						}},
					},
				}
			},
			check: func(t *testing.T, cfg testenv.AgentConfig) {
				testenv.AssertVIPOnLoopback(t, "198.51.100.10", 15*time.Second)
				// The unmanaged VIP must stay off the device. Short
				// timeout — the agent never adds it, so a few seconds
				// is enough to rule out a race with initial reconcile.
				testenv.AssertVIPNotOnLoopback(t, "198.51.100.11", 3*time.Second)
			},
		},
		// per_rule_masquerade (#43 scenario 4).
		//
		// VIP-level masquerade:true with one rule overridden to
		// masquerade:false should produce a postrouting_snat chain with
		// exactly the inheriting rule's backend (and no entry for the
		// overridden rule).
		{
			name: "per_rule_masquerade",
			setup: func(t *testing.T, cfg *testenv.AgentConfig) {
				cfg.PortForwards = []testenv.PortForwardVIPFixture{{
					VIP:        "198.51.100.30",
					Masquerade: true,
					Rules: []testenv.PortForwardRuleFixture{
						{
							Proto: "tcp", Port: 443, DestAddr: "10.1.0.50",
							// Inherits VIP-level masquerade=true.
						},
						{
							Proto: "udp", Port: 53, DestAddr: "10.0.0.100",
							Masquerade: boolPtr(false),
						},
					},
				}}
			},
			check: func(t *testing.T, cfg testenv.AgentConfig) {
				const (
					inheritBackend  = "10.1.0.50"  // remote — should be masqueraded
					overrideBackend = "10.0.0.100" // local — masquerade:false override
				)
				testenv.AssertNftRuleInChain(t, pfTable, "postrouting_snat",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", inheritBackend) &&
							r.HasMatch("tcp", "dport", 443) &&
							r.HasMasquerade()
					},
					15*time.Second, "inheriting rule must appear in postrouting_snat")
				dump := testenv.DumpNftRuleset(t)
				for _, r := range dump.RulesIn(pfTable, "postrouting_snat") {
					if r.HasMatch("ip", "daddr", overrideBackend) {
						t.Errorf("overridden backend %s should not be masqueraded but appears in postrouting_snat: %s",
							overrideBackend, string(r.Raw))
					}
				}
			},
		},
		// multi_vip_separation (#61 scenario 5).
		//
		// Two VIPs, each with their own backend, must produce two
		// independent sets of DNAT and ctzone rules — no cross-pollination.
		// A regression that keyed rules on backend alone (rather than
		// VIP+backend) would cause VIP A's dport-N rule to also accept
		// VIP B's traffic.
		{
			name: "multi_vip_separation",
			setup: func(t *testing.T, cfg *testenv.AgentConfig) {
				cfg.PortForwards = []testenv.PortForwardVIPFixture{
					{
						VIP: "198.51.100.15",
						Rules: []testenv.PortForwardRuleFixture{{
							Proto: "tcp", Port: 80, DestAddr: "10.0.0.150",
						}},
					},
					{
						VIP: "198.51.100.16",
						Rules: []testenv.PortForwardRuleFixture{{
							Proto: "tcp", Port: 80, DestAddr: "10.0.0.160",
						}},
					},
				}
			},
			check: func(t *testing.T, cfg testenv.AgentConfig) {
				const (
					vipA     = "198.51.100.15"
					backendA = "10.0.0.150"
					vipB     = "198.51.100.16"
					backendB = "10.0.0.160"
				)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", vipA) &&
							r.HasMatch("tcp", "dport", 80) &&
							r.HasDNATTo(backendA, 80)
					},
					15*time.Second, "VIP A DNAT must target "+backendA)
				testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
					func(r testenv.NftRule) bool {
						return r.HasMatch("ip", "daddr", vipB) &&
							r.HasMatch("tcp", "dport", 80) &&
							r.HasDNATTo(backendB, 80)
					},
					15*time.Second, "VIP B DNAT must target "+backendB)
				dump := testenv.DumpNftRuleset(t)
				for _, r := range dump.RulesIn(pfTable, "prerouting_dnat") {
					switch {
					case r.HasMatch("ip", "daddr", vipA) && r.HasDNATTo(backendB, 80):
						t.Errorf("VIP A rule must not DNAT to VIP B's backend: %s", string(r.Raw))
					case r.HasMatch("ip", "daddr", vipB) && r.HasDNATTo(backendA, 80):
						t.Errorf("VIP B rule must not DNAT to VIP A's backend: %s", string(r.Raw))
					}
				}
				for _, backend := range []string{backendA, backendB} {
					testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
						func(r testenv.NftRule) bool {
							return r.HasMatch("ip", "saddr", backend) &&
								r.HasMatch("tcp", "sport", 80) &&
								r.HasCTZoneSet(*cfg.PortForwardCTZone)
						},
						10*time.Second, "ctzone reply rule for backend "+backend)
				}
			},
		},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			cfg := startPFScenario(t)
			tc.setup(t, &cfg)
			a := readyAgent(t, cfg)
			defer a.Stop(15 * time.Second)
			tc.check(t, cfg)
		})
	}
}

// TestScenario_PortForwardMultiBackendStickyHashing (#43 scenario 2).
//
// A 3-backend rule should generate a single DNAT statement using nft's jhash
// expression: `dnat to jhash ip saddr mod 3 map { 0:..., 1:..., 2:... }`.
// The map keys are 0-indexed; values pair (addr, port).
func TestScenario_PortForwardMultiBackendStickyHashing(t *testing.T) {
	cfg := startPFScenario(t)

	const vip = "198.51.100.20"
	backends := []string{"10.0.0.100", "10.0.0.101", "10.0.0.102"}
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP: vip,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 8080, DestAddrs: backends,
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	expected := []testenv.NftBackend{
		{Addr: backends[0], Port: 8080},
		{Addr: backends[1], Port: 8080},
		{Addr: backends[2], Port: 8080},
	}
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "daddr", vip) &&
				r.HasMatch("tcp", "dport", 8080) &&
				r.HasDNATMap(expected)
		},
		15*time.Second, "jhash sticky DNAT map across 3 backends")
}

// TestScenario_PortForwardHairpinMasquerade (#43 scenario 5).
//
// hairpin_masquerade:true plus a discovered provider network should produce
// a postrouting_snat rule of the form:
//
//	ip saddr <provider-net> ct original daddr <vip> ct status dnat masquerade
//
// To make the agent discover a provider network we insert a local router
// whose LRP networks fall in 198.51.100.0/24 — the agent's reconcile picks
// these up and feeds them to ReconcilePortForward as the providerNetworks
// arg, which buildNftRuleset uses to materialise the hairpin rule.
func TestScenario_PortForwardHairpinMasquerade(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)

	// Provider network 198.51.100.0/24 — the LRP network the agent will
	// discover and pass into the hairpin rule.
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "hairpinr",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	_ = router

	const vip = "198.51.100.40"
	cfg := pfDefaults(t)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:               vip,
		HairpinMasquerade: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Wait for the reconcile that populates providerNetworks. The agent
	// emits the hairpin rule only on Reconcile (not Setup), so a longer
	// timeout is appropriate.
	testenv.AssertNftRuleInChain(t, pfTable, "postrouting_snat",
		func(r testenv.NftRule) bool {
			// Hairpin rule matches `ip saddr <provider-cidr>` (a
			// prefix expression in nft JSON) plus a masquerade
			// statement. The accompanying ct-expression matches
			// (`ct original daddr <vip>`, `ct status dnat`) are
			// not asserted here — saddr-prefix + masquerade
			// uniquely identify the hairpin rule because no other
			// rule in postrouting_snat carries a CIDR match.
			return r.HasMatchPrefix("ip", "saddr", "198.51.100.0/24") &&
				r.HasMasquerade()
		},
		30*time.Second, "hairpin masquerade rule for VIP "+vip+" with provider 198.51.100.0/24")
}

// TestScenario_PortForwardRouterMasquerade (PR #15).
//
// router_masquerade:true plus a router with a `snat`-type NAT entry should
// produce a postrouting_snat rule of the form:
//
//	ip saddr <router-snat-ip> ct original daddr <vip> ct status dnat masquerade
//
// The agent's OVN reconcile populates state.SNATIPs from NB NAT rows of
// type=snat on locally-active routers; that slice is then handed to
// ReconcilePortForward, which buildNftRuleset turns into the literal-IP
// masquerade rule. Unlike hairpin masquerade (provider-CIDR match) the source
// match here is a single IP, so we assert with HasMatch rather than
// HasMatchPrefix.
func TestScenario_PortForwardRouterMasquerade(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)

	// Local router with LRP in 198.51.100.0/24. The agent's network-filter
	// fallback uses discovered LRP networks, so the SNAT external IP we add
	// below must fall inside this prefix to survive the filter in
	// ovn.go's NAT-collection step.
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "rmasqr",
		LRPMAC:      "fa:16:3e:7a:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		vip     = "198.51.100.41"
		snatIP  = "198.51.100.50"
		snatNet = "10.0.0.0/24"
	)
	testenv.AddSNATEntry(t, ctx, nb, router, snatIP, snatNet)

	cfg := pfDefaults(t)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:              vip,
		RouterMasquerade: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// The rule only appears once OVN has reported at least one SNAT IP, so
	// give reconcile time to run. saddr literal + masquerade uniquely
	// identifies the router-masquerade rule — no other rule in
	// postrouting_snat carries a literal-IP saddr match.
	testenv.AssertNftRuleInChain(t, pfTable, "postrouting_snat",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "saddr", snatIP) &&
				r.HasMasquerade()
		},
		30*time.Second, "router masquerade rule for VIP "+vip+" with SNAT IP "+snatIP)
}

// TestScenario_PortForwardVIPDormantWithoutLocalRouters (#206).
//
// A gateway whose config carries a port-forward VIP but which hosts no local
// routers cannot advertise that VIP: reconcile takes the standby path, which
// empties the FRR prefix-list and the veth-leak network routes. Installing the
// VIP's routes anyway left an FRR static route zebra never selects — the agent
// logged "FRR static routes are configured but inactive" at ERROR on every
// reconcile and held ovn_network_agent_inactive_routes at 1 indefinitely, which
// the shipped alert rule fires on.
//
// The VIP is dormant instead: no kernel route, no FRR route, one info-level
// report, and nothing for the inactive-route check to alarm on. The DNAT plane
// is deliberately still asserted — dormancy withholds the routes, not the
// nftables rules, so a gateway that later gains routers only needs the announce.
func TestScenario_PortForwardVIPDormantWithoutLocalRouters(t *testing.T) {
	// No MakeLocalRouter call: the agent connects to OVN (gateway mode, not
	// port-forward-only — Defaults() sets both remotes) and finds no locally
	// active router, which is exactly the reported condition.
	cfg := startPFScenario(t)

	const vip = "198.51.100.42"
	// manage_vip so the address is the thing under test: a full-mode standby
	// must withhold it — the new dormancy mechanism (#223).
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       vip,
		ManageVIP: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// The DNAT rule still lands — proof the agent reconciled the VIP and did
	// not simply ignore the config block.
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
		func(r testenv.NftRule) bool { return r.HasMatch("ip", "daddr", vip) },
		30*time.Second, "DNAT rule for dormant VIP "+vip)

	// Neither route may appear, and the VIP address must be withheld from the
	// port-forward device — the announce path itself is what dormancy withholds
	// now (#223). All three assertions poll for their whole timeout, so this
	// covers several reconcile cycles at the 5s default interval.
	testenv.AssertNoFRRRoute(t, vip, 15*time.Second)
	testenv.AssertNoKernelRoute(t, vip, 5*time.Second)
	testenv.AssertVIPNotOnLoopback(t, vip, 15*time.Second)

	logs := a.LogTail(100000)
	if strings.Contains(logs, "FRR static routes are configured but inactive") {
		t.Errorf("dormant VIP must not raise the inactive-route alarm; last logs:\n%s", a.LogTail(40))
	}
	if !strings.Contains(logs, "port-forward VIPs are dormant on this node") {
		t.Errorf("expected the dormancy to be reported once at info level; last logs:\n%s", a.LogTail(40))
	}
}

// TestScenario_PortForwardVIPAnnouncedViaConnected (#223).
//
// The active-gateway counterpart of the dormant scenario, and the direct pin
// for the bug this issue fixes. On a gateway that owns a local router the
// managed VIP is announceable, so the agent installs its /32 address on
// port_forward_dev (whose connected route is the BGP announce path) and its
// kernel /32 on the provider bridge — but writes no FRR static for it. The
// pre-fix agent wrote that static and the connected route permanently shadowed
// it, so the VIP was never advertised while the agent logged the inactive-route
// ERROR every cycle. Here the static must be absent *while active*, no
// inactive-route alarm may fire, and the two other planes (address, kernel
// route) must be present.
func TestScenario_PortForwardVIPAnnouncedViaConnected(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)
	testenv.ScrubPortForwardResidue(t)

	// A locally-active router makes this gateway active, so its managed VIP is
	// announceable — the condition under which the bug manifested.
	_ = testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "pfannouncer",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const vip = "198.51.100.50"
	cfg := pfDefaults(t)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       vip,
		ManageVIP: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)
	// A SIGKILL from a failing assertion would leave nft / loopback residue.
	t.Cleanup(func() { testenv.ScrubPortForwardResidue(t) })

	// The announce path exists: the VIP address on the port-forward device
	// (its connected route is what BGP redistributes) and the kernel /32 on the
	// provider bridge.
	testenv.AssertVIPOnLoopback(t, vip, 15*time.Second)
	testenv.AssertKernelRoute(t, vip, 15*time.Second)

	// The shadowed static must be gone while the gateway is active — the actual
	// bug. The negative assertion polls its whole timeout, covering several
	// reconcile cycles at the 5s default interval.
	testenv.AssertNoFRRRoute(t, vip, 15*time.Second)

	// And no inactive-route alarm: with the VIP out of the FRR-static set,
	// nothing sits in FRR unadvertised holding ovn_network_agent_inactive_routes
	// non-zero.
	logs := a.LogTail(100000)
	if strings.Contains(logs, "FRR static routes are configured but inactive") {
		t.Errorf("an announced VIP must not raise the inactive-route alarm; last logs:\n%s", a.LogTail(40))
	}
}

// TestScenario_PortForwardL3mdevSysctls (#43 scenario 6).
//
// port_forward_l3mdev_accept:true must set udp_l3mdev_accept=1 and
// tcp_l3mdev_accept=1 in /proc/sys/net/ipv4/. These are GLOBAL sysctls — the
// test saves and restores them via SaveSysctl so the host is left as it was
// found.
func TestScenario_PortForwardL3mdevSysctls(t *testing.T) {
	cfg := startPFScenario(t)

	// Capture original values so the t.Cleanup restores them after the
	// test, regardless of pass/fail.
	testenv.SaveSysctl(t, testenv.SysctlUDPL3mdevAccept)
	testenv.SaveSysctl(t, testenv.SysctlTCPL3mdevAccept)

	cfg.PortForwardL3mdevAccept = boolPtr(true)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP: "198.51.100.50",
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	testenv.AssertSysctl(t, testenv.SysctlUDPL3mdevAccept, "1", 10*time.Second)
	testenv.AssertSysctl(t, testenv.SysctlTCPL3mdevAccept, "1", 10*time.Second)
}

// TestScenario_PortForwardOutputChains (#43 scenario 7).
//
// A local-backend rule with masquerade:false should leave both output_ctzone
// (zone assignment for locally-generated reply traffic) and output_fwmark
// (mark 0x200 with type=route to trigger reroute) chains in place, with rules
// mirroring their prerouting counterparts. The agent always emits these
// chains, but the rules inside them must include the local-backend's port —
// otherwise the same-host reply path is broken.
func TestScenario_PortForwardOutputChains(t *testing.T) {
	cfg := startPFScenario(t)

	const (
		vip          = "198.51.100.60"
		localBackend = "127.0.0.10"
	)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:        vip,
		Masquerade: false, // local backend, no SNAT
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 8443, DestAddr: localBackend,
			Masquerade: boolPtr(false),
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// output_ctzone exists and mirrors the original-direction zone rule.
	testenv.AssertNftChainExists(t, pfTable, "output_ctzone", 15*time.Second)
	testenv.AssertNftRuleInChain(t, pfTable, "output_ctzone",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "daddr", vip) &&
				r.HasMatch("tcp", "dport", 8443) &&
				r.HasCTZoneSet(*cfg.PortForwardCTZone)
		},
		10*time.Second, "output_ctzone mirrors VIP daddr rule")
	testenv.AssertNftRuleInChain(t, pfTable, "output_ctzone",
		func(r testenv.NftRule) bool {
			return r.HasMatch("ip", "saddr", localBackend) &&
				r.HasMatch("tcp", "sport", 8443) &&
				r.HasCTZoneSet(*cfg.PortForwardCTZone)
		},
		10*time.Second, "output_ctzone mirrors backend saddr rule")

	// output_fwmark exists and sets the reply-direction mark.
	testenv.AssertNftChainExists(t, pfTable, "output_fwmark", 15*time.Second)
	testenv.AssertNftRuleInChain(t, pfTable, "output_fwmark",
		func(r testenv.NftRule) bool { return r.HasMarkSet(testenv.DefaultDnatReplyFwmark) },
		10*time.Second, "output_fwmark sets reply-direction mark on locally-generated reply")

	// Verify the chain header is `type route` (not `type filter`) — the
	// route hook is required to trigger routing re-evaluation when the
	// mark changes. Without it the reply never re-enters the provider VRF.
	chain, ok := testenv.DumpNftRuleset(t).Chain(pfTable, "output_fwmark")
	if !ok {
		t.Fatal("output_fwmark chain missing in second dump")
	}
	if chain.Type != "route" {
		t.Errorf("output_fwmark chain type = %q, want %q", chain.Type, "route")
	}
}

// TestScenario_PortForwardCleanupOnSIGTERM (#43 scenario 8).
//
// SIGTERM with cleanup_on_shutdown=true (the harness default) must remove
// the agent's nft table and all VIP addresses managed by manage_vip:true.
// We start the agent, verify the desired state landed, send SIGTERM via
// AgentProc.Stop, and assert both the table and the VIP are gone.
func TestScenario_PortForwardCleanupOnSIGTERM(t *testing.T) {
	cfg := startPFScenario(t)
	// Port-forward-only mode (both remotes cleared): pf-only is where a
	// manage_vip address is added unconditionally, so there is a managed VIP to
	// clean up. Routerless full mode would now withhold it as dormant (#223).
	cfg.OVNNBRemote = ""
	cfg.OVNSBRemote = ""

	const vip = "198.51.100.99"
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       vip,
		ManageVIP: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.99",
		}},
	}}

	a := readyAgent(t, cfg)

	// Confirm we have something to clean up before signalling SIGTERM.
	testenv.AssertNftChainExists(t, pfTable, "prerouting_dnat", 15*time.Second)
	testenv.AssertVIPOnLoopback(t, vip, 15*time.Second)

	if err := a.Stop(15 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	testenv.AssertNftTableAbsent(t, pfTable, 10*time.Second)
	testenv.AssertVIPNotOnLoopback(t, vip, 10*time.Second)
}

// =============================================================================
// Scenarios from #61 — additional port-forward coverage.
//
// The eight scenarios from #43 nail down the structural shape but several
// operationally important variations remained unchecked: UDP, port
// translation, the fwmark / policy-routing plumbing, and how hairpin
// masquerade interacts with per-rule masquerade overrides. The tests below
// fill those gaps.
// =============================================================================

// TestScenario_PortForwardFwmarkPolicyRoutes (#61 scenario 3).
//
// With any single-backend rule installed, the agent's ensureDNATRouting must
// produce two ip rules and a route in the configured port-forward table:
//
//   - DefaultDnatFwmarkPriority: DefaultDnatFwmark → lookup main (forward)
//   - DefaultDnatReplyPriority:  DefaultDnatReplyFwmark → lookup <pf-table>
//   - <pf-table>:                default via <veth-default> (return path)
//
// We assert all three using the JSON-parsed helpers added in this commit so
// the test is robust against iproute2 version drift.
//
// The default port_forward_table_id is DefaultPortForwardTableID — we use
// the default here so scrubPortForwardState (which flushes that table)
// reaches the route on teardown.
func TestScenario_PortForwardFwmarkPolicyRoutes(t *testing.T) {
	cfg := startPFScenario(t)

	const (
		vip     = "198.51.100.14"
		backend = "10.0.0.130"
	)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP: vip,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: backend,
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Forward rule: forward-direction fwmark → lookup main. iproute2 emits
	// the main table by name, not number.
	testenv.AssertIPRulePriority(t,
		testenv.DefaultDnatFwmarkPriority,
		testenv.DefaultDnatFwmark,
		"main", 15*time.Second)

	// Reply rule: reply-direction fwmark → lookup the PF table.
	testenv.AssertIPRulePriority(t,
		testenv.DefaultDnatReplyPriority,
		testenv.DefaultDnatReplyFwmark,
		strconv.Itoa(testenv.DefaultPortForwardTableID),
		15*time.Second)

	// Reply table route: agent installs `default via <vethProviderIP> dev
	// veth-default`. The issue references "loopback1 (or whatever the agent
	// emits as its return path)" — the production return path is the veth
	// pair so we assert on veth-default. We do not pin the gateway IP so
	// the test stays robust against operators reconfiguring veth-nexthop.
	testenv.AssertRouteInTable(t,
		strconv.Itoa(testenv.DefaultPortForwardTableID),
		"default", "veth-default", 15*time.Second)
}

// TestScenario_PortForwardHairpinPlusPerRuleMasquerade (#61 scenario 4).
//
// A VIP with hairpin_masquerade:true and one rule overridden to
// masquerade:false should produce the hairpin rule (ip saddr <provider-cidr>
// masquerade) but NOT a per-backend SNAT entry for the overridden rule.
//
// Why this matrix matters: hairpin_masquerade and per-rule masquerade live
// in two different code paths inside buildNftRuleset. Overriding the rule
// to masquerade:false should suppress the backend-targeted SNAT without
// disturbing the provider-CIDR hairpin rule. Without this test a refactor
// that conflated the two flags could silently break either case.
func TestScenario_PortForwardHairpinPlusPerRuleMasquerade(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)

	// Provider network 198.51.100.0/24 — the LRP network the agent will
	// discover and pass into the hairpin rule, mirroring the existing
	// TestScenario_PortForwardHairpinMasquerade setup.
	_ = testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "hpoverrider",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		vip             = "198.51.100.41"
		overrideBackend = "10.0.0.140"
	)
	cfg := pfDefaults(t)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:               vip,
		HairpinMasquerade: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: overrideBackend,
			Masquerade: boolPtr(false),
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Hairpin rule must still appear.
	testenv.AssertNftRuleInChain(t, pfTable, "postrouting_snat",
		func(r testenv.NftRule) bool {
			return r.HasMatchPrefix("ip", "saddr", "198.51.100.0/24") &&
				r.HasMasquerade()
		},
		30*time.Second, "hairpin masquerade rule for "+vip+" with provider 198.51.100.0/24")

	// Per-rule override (masquerade:false) must NOT add a backend-targeted
	// SNAT. Read the dump once — by this point the hairpin assertion has
	// already converged.
	dump := testenv.DumpNftRuleset(t)
	for _, r := range dump.RulesIn(pfTable, "postrouting_snat") {
		if r.HasMatch("ip", "daddr", overrideBackend) {
			t.Errorf("override backend %s should not be masqueraded but appears in postrouting_snat: %s",
				overrideBackend, string(r.Raw))
		}
	}
}
