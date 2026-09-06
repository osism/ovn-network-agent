//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// All scenarios in this file cover #56 — the agent's veth-leak lifecycle.
//
// Three pieces of state are under test:
//
//   - SetupVethLeak: a `veth-default`/`veth-provider` pair created on agent
//     startup. veth-provider is enslaved to vrf-provider; the leak table
//     (default 200) holds the default route via veth-default.
//   - ReconcileVethLeakNetworks: per-network VRF routes (proto 44 in the VRF's
//     table) and per-network policy rules at the agent's priority (default
//     2000), reconciled every cycle from the locally-active routers' LRP
//     networks.
//   - TeardownVethLeak: on SIGTERM, the agent's cleanup() removes the policy
//     rules, leak-table default route, per-network VRF routes, and finally
//     the veth pair itself.
//
// Without coverage here, the surface area is only exercised transitively by
// the FIP and port-forward scenarios, which never assert on the actual
// netlink state. A regression that, e.g., dropped the proto-44 tag or
// stopped reconciling stale ip-rules would slip through CI.
//
// Each scenario uses testenv.FastDefaults() (2s reconcile) where the test
// observes drift recovery; otherwise testenv.Defaults() (5s) keeps timing
// closer to production.

// TestScenario_VethLeakSetup (#56 scenario 1):
//
// Cold start: agent connects, runs initial reconcile, becomes ready. By the
// time WaitReady returns the veth pair must exist, both ends must be up,
// veth-provider must be enslaved to vrf-provider, and the leak table must
// hold the agent's default route.
//
// SetupVethLeak runs before OVN.Connect (agent.go:90), so this assertion
// holds regardless of OVN logical state. There is no local router in this
// scenario, so per-network entries are not expected.
func TestScenario_VethLeakSetup(t *testing.T) {
	_, cancel, _, _ := startScenario(t)
	defer cancel()

	testenv.WithAgent(t, testenv.Defaults())

	testenv.AssertVethPairPresent(t, 5*time.Second)
	testenv.AssertVethDefaultRouteInLeakTable(t, 5*time.Second)
	// The ingress exception (#265) is part of the pair's setup, not of any
	// network: it must be there before a per-network rule ever is.
	testenv.AssertVethIngressRule(t, testenv.VethIngressRulePriority, 5*time.Second)
}

// TestScenario_VethLeakReconcileNetwork (#56 scenario 2):
//
// With one locally-active router carrying LRP 198.51.100.11/24, the agent's
// next reconcile must install:
//
//   - 198.51.100.0/24 via 169.254.0.1 dev veth-provider proto 44 in the VRF
//     table
//   - ip rule from 198.51.100.0/24 lookup <leak-table> priority <agent-priority>
//
// The router is created before agent start so the startup reconcile observes
// it; that path also exercises SetupVethLeak's "if len(networkFilters) > 0,
// run an initial ReconcileVethLeakNetworks" branch (routing_linux.go:471).
func TestScenario_VethLeakReconcileNetwork(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const network = "198.51.100.0/24"
	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "vethnet",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	testenv.WithAgent(t, testenv.Defaults())

	testenv.AssertVethPairPresent(t, 5*time.Second)
	testenv.AssertVethRouteInVRF(t, network, 15*time.Second)
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, network, 15*time.Second)
}

// TestScenario_VethLeakDriftRecovery (#56 scenario 3):
//
// With the per-network rule installed, deleting it out of band must be
// detected by the periodic reconcile and re-installed within at most a
// couple of ticks. The agent's recovery path is the policy-rule sweep at
// the end of ReconcileVethLeakNetworks (routing_linux.go:678).
//
// FastDefaults ticks at 2s, so we allow up to ~3 ticks before failing —
// drift recovery is a best-effort property and asserting on a single tick
// would be flaky on a busy CI runner.
func TestScenario_VethLeakDriftRecovery(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const network = "198.51.100.0/24"
	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "vethdrift",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	testenv.WithAgent(t, testenv.FastDefaults())

	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, network, 10*time.Second)

	// Drift: rip the policy rule out from under the agent. The next
	// reconcile cycle must observe it as missing and reinstall it.
	testenv.DeleteIPRule(t, testenv.DefaultVethLeakRulePriority, network)

	// Verify the deletion took effect — otherwise the re-add assertion
	// below would pass trivially. Use a tight inner timeout so we fail fast
	// if `ip rule del` did not actually remove the rule.
	testenv.AssertNoIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, network, 1*time.Second)

	// Re-appears within reconcile_interval (2s) + processing slack.
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, network, 8*time.Second)
}

// TestScenario_VethLeakStaleNetworkRemoval (#56 scenario 4):
//
// With two locally-active routers carrying distinct LRP networks, both
// per-network routes and rules must be present. After deleting the second
// router the agent must reap its entries within a couple of ticks while
// leaving the first router's state untouched.
//
// The reaping path is the "Remove stale per-network routes and rules" loop
// in ReconcileVethLeakNetworks (routing_linux.go:653). It compares
// currentNets to desiredSet and deletes anything not in the desired set.
func TestScenario_VethLeakStaleNetworkRemoval(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const (
		net1 = "198.51.100.0/24"
		net2 = "203.0.113.0/24"
	)
	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "vethstale1",
		LRPMAC:      "fa:16:3e:11:22:33",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	r2 := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "vethstale2",
		LRPMAC:      "fa:16:3e:11:22:44",
		LRPNetworks: []string{"203.0.113.11/24"},
	})

	testenv.WithAgent(t, testenv.FastDefaults())

	// Both networks must show up.
	testenv.AssertVethRouteInVRF(t, net1, 15*time.Second)
	testenv.AssertVethRouteInVRF(t, net2, 15*time.Second)
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, net1, 5*time.Second)
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, net2, 5*time.Second)

	// Remove the second router. The agent observes net2 as no longer
	// in the desired set and reaps its route + rule.
	deleteRouter(t, ctx, nb, r2.RouterUUID)

	testenv.AssertNoVethRouteInVRF(t, net2, 15*time.Second)
	testenv.AssertNoIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, net2, 5*time.Second)

	// net1 must remain — the test fails if the agent over-reaped.
	testenv.AssertVethRouteInVRF(t, net1, 1*time.Second)
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, net1, 1*time.Second)
}

// TestScenario_VethLeakTeardownOnSigterm (#56 scenario 5):
//
// With cleanup_on_shutdown=true (the agent default) and a per-network entry
// installed, SIGTERM must cause TeardownVethLeak to:
//
//   - remove all policy rules at the agent's priority
//   - flush proto-44 routes from the VRF table
//   - delete the veth pair (one end suffices — the kernel removes both)
//
// We start a fresh agent so each phase is observable in isolation: install,
// then SIGTERM, then assert teardown completed. The scenario uses RunAgent
// + manual Stop rather than WithAgent because WithAgent's t.Cleanup runs
// after t returns, which is too late for assertions on the post-SIGTERM
// state.
func TestScenario_VethLeakTeardownOnSigterm(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const network = "198.51.100.0/24"
	testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "vethteardown",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	cfg := testenv.Defaults()
	cleanup := true
	cfg.CleanupOnShutdown = &cleanup

	a := readyAgent(t, cfg)

	testenv.AssertVethPairPresent(t, 5*time.Second)
	testenv.AssertVethRouteInVRF(t, network, 15*time.Second)
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, network, 5*time.Second)

	if err := a.Stop(20 * time.Second); err != nil {
		t.Fatalf("agent stop: %v", err)
	}

	// Veth pair gone, per-network route gone, per-network rule gone, and
	// the ingress exception rule gone with them.
	testenv.AssertNoVethPair(t, 5*time.Second)
	testenv.AssertNoVethRouteInVRF(t, network, 5*time.Second)
	testenv.AssertNoIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, network, 5*time.Second)
	testenv.AssertNoVethIngressRule(t, 5*time.Second)
}

// TestScenario_VethLeakIngressRuleDriftRecovery (#265):
//
// With a FIP whose /32 lives in the main table (route_table_id unset), the
// ingress exception rule `iif veth-default lookup main` must be back within
// a couple of ticks after an out-of-band `ip rule del`, and the repair must
// not touch the healthy kernel route. The repair path is the
// ensureVethIngressRule call at the top of ReconcileVethLeakNetworks.
func TestScenario_VethLeakIngressRuleDriftRecovery(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const fip = "198.51.100.43"
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "vethingress",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.43")

	testenv.WithAgent(t, testenv.FastDefaults())

	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertVethIngressRule(t, testenv.VethIngressRulePriority, 10*time.Second)

	// Drift: remove only the rule. Verify the deletion took effect so the
	// recovery assertion cannot pass trivially.
	testenv.DeleteVethIngressRule(t, testenv.VethIngressRulePriority)
	testenv.AssertNoVethIngressRule(t, 1*time.Second)

	// Re-appears within reconcile_interval (2s) + processing slack, and the
	// route it protects was never replaced.
	testenv.AssertVethIngressRule(t, testenv.VethIngressRulePriority, 8*time.Second)
	testenv.AssertKernelRoute(t, fip, 1*time.Second)
}

// TestScenario_VethLeakIngressNoLoop (#265):
//
// The forwarding proof on one host. A packet sourced inside the leaked
// provider prefix is sent from vrf-provider towards a FIP hosted here; it
// leaves via veth-provider and enters the default VRF on veth-default, which
// is exactly what a peer gateway's VRF hands over for cross-chassis
// FIP-to-FIP traffic. Without the ingress exception the source rule sends it
// straight back into the VRF and each packet crosses veth-default about 32
// times before its TTL runs out; with it the packet leaves towards br-ex once
// and never returns to the veth pair. The veth-default TX counter tells the
// two apart.
func TestScenario_VethLeakIngressNoLoop(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	const (
		network = "198.51.100.0/24"
		fip     = "198.51.100.43"
		src     = "198.51.100.99"
		probes  = 3
	)
	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "vethnoloop",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.43")

	testenv.WithAgent(t, testenv.FastDefaults())

	// Every leg of the path the probe takes must be in place first: the /32
	// in main, the source rule that would capture the packet, and the FRR
	// static that steers it out of the VRF through the veth pair.
	testenv.AssertKernelRoute(t, fip, 10*time.Second)
	testenv.AssertIPRuleAtPriority(t, testenv.DefaultVethLeakRulePriority, network, 10*time.Second)
	testenv.AssertFRRRoute(t, fip, 15*time.Second)
	testenv.AssertVethIngressRule(t, testenv.VethIngressRulePriority, 5*time.Second)

	before := testenv.VethLinkTxPackets(t, testenv.VethDefaultName)
	testenv.PingFromVRF(t, src, fip, probes)
	after := testenv.VethLinkTxPackets(t, testenv.VethDefaultName)

	// A looping probe packet would add ~32 transmissions on veth-default;
	// three of them well over 90. Allow a few packets of unrelated chatter.
	const maxDelta = 3 * probes
	if delta := after - before; delta > maxDelta {
		t.Fatalf("veth-default TX grew by %d packets for %d probes (max %d): provider-sourced ingress is looping through the veth pair",
			delta, probes, maxDelta)
	}
}

// TestScenario_VethLeakDisabledSkip (#56 scenario 6 — negative):
//
// With veth_leak_enabled=false the agent must skip SetupVethLeak entirely.
// Counter-asserts scenario #1: even after the agent has been ready long
// enough that any cold-start setup would have completed, neither end of
// the veth pair must exist.
//
// PortForwardEnabled is left empty because port forwarding requires
// veth_leak_enabled=true (config.go:430) — config validation would reject
// this combination.
func TestScenario_VethLeakDisabledSkip(t *testing.T) {
	_, cancel, _, _ := startScenario(t)
	defer cancel()

	cfg := testenv.Defaults()
	disabled := false
	cfg.VethLeakEnabled = &disabled

	a := testenv.RunAgent(t, cfg)
	rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer rcancel()
	if err := a.WaitReady(rctx); err != nil {
		t.Fatalf("agent did not become ready: %v", err)
	}
	defer a.Stop(15 * time.Second)

	// The agent reached "agent running" without setting up the pair —
	// hold for a fraction of a reconcile interval to make sure the
	// negative still holds across the next tick. The ingress exception
	// rule belongs to the pair and must be absent with it.
	testenv.AssertNoVethPair(t, 1*time.Second)
	testenv.AssertNoVethIngressRule(t, 1*time.Second)
}
