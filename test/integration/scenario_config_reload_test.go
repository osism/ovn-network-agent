//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #91: the SIGHUP configuration reload. Each
// one starts the real agent binary, rewrites its config file and sends SIGHUP
// through AgentProc.Reload, then asserts on the host state, the metrics
// endpoint and the log. The reload is applied on the agent's main loop, so
// every assertion carries a timeout.

const reloadMetric = "ovn_network_agent_config_reload_total"

// pfOnlyReloadConfig returns a port-forward-only config (both OVN remotes
// cleared, as the manage_vip matrix row does) with the metrics endpoint on a
// free loopback port and one managed tcp/443 VIP per address.
func pfOnlyReloadConfig(t *testing.T, vips ...string) (testenv.AgentConfig, string) {
	t.Helper()
	cfg := startPFScenario(t)
	cfg.OVNNBRemote = ""
	cfg.OVNSBRemote = ""
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	for _, vip := range vips {
		cfg.PortForwards = append(cfg.PortForwards, testenv.PortForwardVIPFixture{
			VIP:       vip,
			ManageVIP: true,
			Rules: []testenv.PortForwardRuleFixture{{
				Proto: "tcp", Port: 443, DestAddr: "10.0.0.100",
			}},
		})
	}
	return cfg, addr
}

// assertReloadCount waits for config_reload_total{outcome} to reach want.
func assertReloadCount(t *testing.T, addr, outcome string, want float64) {
	t.Helper()
	testenv.AssertMetricEventually(t, addr, reloadMetric,
		map[string]string{"outcome": outcome},
		func(v float64, present bool) bool { return present && v == want },
		10*time.Second)
}

// assertLogContains waits for the agent log to contain every substring on one
// line.
func assertLogContains(t *testing.T, a *testenv.AgentProc, substrings ...string) {
	t.Helper()
	testenv.Eventually(t, func() bool {
		for _, line := range strings.Split(a.LogTail(500), "\n") {
			all := true
			for _, s := range substrings {
				if !strings.Contains(line, s) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "agent log line containing "+strings.Join(substrings, " + "))
}

// hasDNATRuleFor reports whether prerouting_dnat carries a rule for vip.
func hasDNATRuleFor(d testenv.NftDump, vip string) bool {
	for _, r := range d.RulesIn(pfTable, "prerouting_dnat") {
		if r.HasMatch("ip", "daddr", vip) {
			return true
		}
	}
	return false
}

// TestScenario_ReloadPortForwardVIPRemoved: dropping a managed VIP from
// port_forwards and reloading must withdraw its address from loopback1 (the
// per-cycle reconcile only walks the current list, so nothing else would) and
// its DNAT rule, while the remaining VIP stays in service.
func TestScenario_ReloadPortForwardVIPRemoved(t *testing.T) {
	const keep, drop = "198.51.100.30", "198.51.100.31"
	cfg, addr := pfOnlyReloadConfig(t, keep, drop)

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)
	testenv.AssertVIPOnLoopback(t, keep, 15*time.Second)
	testenv.AssertVIPOnLoopback(t, drop, 15*time.Second)

	cfg.PortForwards = cfg.PortForwards[:1]
	a.Reload(cfg)

	testenv.AssertVIPNotOnLoopback(t, drop, 10*time.Second)
	testenv.EventuallyNft(t, func(d testenv.NftDump) bool { return !hasDNATRuleFor(d, drop) },
		10*time.Second, "DNAT rule for the dropped VIP removed")
	testenv.AssertVIPOnLoopback(t, keep, 2*time.Second)
	assertReloadCount(t, addr, "success", 1)
}

// TestScenario_ReloadInvalidConfigKeepsRunning: a reload whose file fails
// validation must leave the agent running on its previous configuration.
func TestScenario_ReloadInvalidConfigKeepsRunning(t *testing.T) {
	const vip = "198.51.100.30"
	cfg, addr := pfOnlyReloadConfig(t, vip)

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)
	testenv.AssertVIPOnLoopback(t, vip, 15*time.Second)

	bad := cfg
	bad.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       vip,
		ManageVIP: true,
		Rules:     []testenv.PortForwardRuleFixture{{Proto: "tcp", Port: 70000, DestAddr: "10.0.0.100"}},
	}}
	a.Reload(bad)

	assertLogContains(t, a, "configuration reload failed", "invalid port 70000")
	assertReloadCount(t, addr, "error", 1)
	if !a.Alive() {
		t.Fatalf("agent exited after an invalid reload (logs: %s)", a.LogTail(20))
	}
	testenv.AssertVIPOnLoopback(t, vip, 2*time.Second)
	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_dnat",
		func(r testenv.NftRule) bool { return r.HasMatch("ip", "daddr", vip) },
		2*time.Second, "DNAT rule kept after an invalid reload")
}

// TestScenario_ReloadRestartOnlyKeySkipped: a restart-only key changed in the
// file is reported and kept at its running value, even across the next
// reconcile.
func TestScenario_ReloadRestartOnlyKeySkipped(t *testing.T) {
	const vip = "198.51.100.30"
	cfg, addr := pfOnlyReloadConfig(t, vip)
	cfg.PortForwardCTZone = intPtr(64000)

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	changed := cfg
	changed.PortForwardCTZone = intPtr(64001)
	a.Reload(changed)

	assertLogContains(t, a, "configuration change needs a restart", "key=port_forward_ct_zone")
	assertReloadCount(t, addr, "success", 1)

	// Let a periodic reconcile rewrite the ruleset after the reload.
	periodic := map[string]string{"trigger": "periodic"}
	before, _ := testenv.ScrapeMetrics(t, addr).Value("ovn_network_agent_reconcile_total", periodic)
	testenv.AssertMetricEventually(t, addr, "ovn_network_agent_reconcile_total", periodic,
		func(v float64, present bool) bool { return present && v > before },
		15*time.Second)

	testenv.AssertNftRuleInChain(t, pfTable, "prerouting_ctzone",
		func(r testenv.NftRule) bool { return r.HasCTZoneSet(64000) },
		2*time.Second, "ct zone still the running 64000")
	for _, r := range testenv.DumpNftRuleset(t).RulesIn(pfTable, "prerouting_ctzone") {
		if r.HasCTZoneSet(64001) {
			t.Fatal("prerouting_ctzone uses the restart-only 64001 after a reload")
		}
	}
}

// TestScenario_ReloadFRRPrefixListRename: renaming frr_prefix_list empties the
// previous list and fills the new one on the reconcile the reload triggers.
func TestScenario_ReloadFRRPrefixListRename(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const (
		listA   = "OVN-AGENT-TEST-91-A"
		listB   = "OVN-AGENT-TEST-91-B"
		network = "198.51.100.0/24"
	)
	prefixListCleanup(t, listA)
	prefixListCleanup(t, listB)

	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "reloadrename",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cfg.FRRPrefixList = listA
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)
	testenv.AssertFRRPrefixListContains(t, listA, network, 15*time.Second)

	cfg.FRRPrefixList = listB
	a.Reload(cfg)

	testenv.AssertFRRPrefixListContains(t, listB, network, 15*time.Second)
	testenv.AssertFRRPrefixListEmpty(t, listA, 15*time.Second)
}

// TestScenario_ReloadReconcileInterval: a reloaded reconcile_interval resets
// the running ticker.
func TestScenario_ReloadReconcileInterval(t *testing.T) {
	cfg, addr := pfOnlyReloadConfig(t, "198.51.100.30")
	cfg.ReconcileInterval = "1h"

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	cfg.ReconcileInterval = "1s"
	a.Reload(cfg)

	testenv.AssertMetricEventually(t, addr, "ovn_network_agent_reconcile_total",
		map[string]string{"trigger": "periodic"},
		func(v float64, present bool) bool { return present && v >= 2 },
		10*time.Second)
}
