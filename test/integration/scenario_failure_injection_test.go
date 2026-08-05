//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// Scenarios in this file cover #88 item 1: a transient failure of one of the
// three external tools the agent shells out to (vtysh, nft, ovs-ofctl)
// followed by self-heal on the next periodic reconcile. The shim helper
// (testenv.WithFailingTool) is the common substrate — each sub-scenario arms
// it at the moment that lets the agent observe the failure, then asserts the
// desired state arrives after the shim disarms itself.
//
// All scenarios in this file share the TestScenario_FailureInjection_ prefix
// so acceptance criterion #3 (no flakes on 10 consecutive runs of
// `go test -count=10 -tags=integration -run TestScenario_FailureInjection_`)
// can target the whole set with one -run argument.

// TestScenario_FailureInjection_VtyshFailsOnce (#88 item 1, vtysh sub-case):
//
// Arm vtysh to fail every call in the next reconcile cycle. The agent's
// initial reconcile then cannot install the FRR /32 route for a fresh FIP,
// but the next periodic tick — by which point the shim has exhausted its
// counter and disarmed — re-runs ensureRoutes and the route appears.
//
// We pick the shim's failCount high enough to also blow through verifyRoutes,
// which runs ListFRRRoutes and AddFRRRoutes again after the initial add. Three
// failures (List, Add, verify-List) is enough to keep FRR empty for the whole
// cycle; the fourth call (the second cycle's List) finds the counter zero and
// chains through to real vtysh.
//
// The kernel route is installed unconditionally — netlink doesn't go through
// the shim — so the test pins it down separately as a sanity check that only
// the FRR plane experienced the failure.
func TestScenario_FailureInjection_VtyshFailsOnce(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fivtysh",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	shim := testenv.WithFailingTool(t, "vtysh", 3)
	cfg := testenv.FastDefaults()
	cfg.ExtraEnv = append(cfg.ExtraEnv, shim.Env())
	shim.Arm()

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	const fipExternal = "198.51.100.42"
	testenv.AddFIP(t, ctx, nb, router, fipExternal, "10.0.0.42")

	// Kernel route lands regardless of vtysh state — its install path is
	// pure netlink. Asserting it first proves the agent saw the new FIP.
	testenv.AssertKernelRoute(t, fipExternal, 15*time.Second)

	// FRR route appears once the shim's counter exhausts and the next
	// periodic reconcile retries the install. ReconcileInterval is 2s in
	// FastDefaults, so two cycles fit comfortably inside 20s.
	testenv.AssertFRRRoute(t, fipExternal, 20*time.Second)

	// The shim wrote a marker line on every forced failure. Verify at
	// least one made it to the agent log — otherwise we recovered without
	// actually hitting the failure path the test claims to exercise.
	logs := a.LogTail(100000)
	if !strings.Contains(logs, "test shim: forced failure of vtysh") {
		t.Errorf("expected forced-failure marker for vtysh in agent log; tail:\n%s", a.LogTail(40))
	}
}

// TestScenario_FailureInjection_NftConflict (#88 item 1, nft sub-case):
//
// The agent's SetupPortForward path is fatal on nft failure, so we let the
// agent come up cleanly before arming the shim. Once armed, the next
// reconcile's `nft -f -` fails — but the agent already had a valid ruleset
// installed at setup, so we induce a fresh `nft -f -` by deleting the table
// out-of-band and waiting for the next periodic reconcile to detect drift
// and re-apply. After the shim disarms (counter exhausted), the same
// reconcile succeeds and the table reappears.
//
// The shim is filtered to `nft -f -` invocations only. The agent's
// applyNftRuleset first runs `nft list table ip <name>` to decide whether
// to prepend a `delete table` line; if that list call fails the agent
// silently treats it as "table missing" and the subsequent ruleset load
// happens anyway. Targeting `-f -` specifically guarantees the failure
// hits the load step, which is the only step that maps to a reconcile-
// level error in agent logs.
//
// The "syntactically valid but semantically conflicting prior rule" the
// issue suggests is realised here as a shim-injected non-zero exit from
// `nft -f -`. The reconcile path under test is the same regardless of
// whether nft rejected the ruleset because of an external conflict or
// because the binary itself was forced to fail.
func TestScenario_FailureInjection_NftConflict(t *testing.T) {
	cfg := startPFScenario(t)

	const vip = "198.51.100.71"
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       vip,
		ManageVIP: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.100",
		}},
	}}
	cfg.ReconcileInterval = "2s"

	shim := testenv.WithFailingTool(t, "nft", 1).MatchArg("-f -")
	cfg.ExtraEnv = append(cfg.ExtraEnv, shim.Env())

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)
	t.Cleanup(func() { testenv.ScrubPortForwardResidue(t) })

	// Sanity: the agent installed its table during setup, since the shim
	// is still disarmed at this point.
	testenv.AssertNftChainExists(t, testenv.DefaultNftTable, "prerouting_dnat", 15*time.Second)

	// Delete the table out-of-band so the next reconcile is forced to
	// re-apply the ruleset. Then arm the shim so the very next `nft -f -`
	// — i.e. that re-apply — fails. The shim's counter is 1, so exactly
	// one apply is rejected before subsequent calls chain through.
	if out, err := exec.Command("nft", "delete", "table", "ip", testenv.DefaultNftTable).CombinedOutput(); err != nil {
		t.Fatalf("pre-arm delete table: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	shim.Arm()

	// Wait for the agent to attempt the re-apply and fail at least once.
	// Capture the log marker the shim writes so a regression that doesn't
	// hit the failure path cannot pass silently.
	//
	// On timeout, dump the shim's invocation log: a non-empty log proves
	// the shim is on PATH and being reached (so the bug is in the
	// arm/counter logic), an empty log says PATH override never landed
	// (the env-propagation path is the suspect).
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("nft shim invocation log:\n%s", shim.InvocationLog())
		}
	})
	testenv.Eventually(t, func() bool {
		return strings.Contains(a.LogTail(100000), "test shim: forced failure of nft")
	}, 15*time.Second, 200*time.Millisecond, "agent must hit at least one forced nft failure")

	// After the shim disarms (counter hit zero), the next reconcile's
	// nft -f - succeeds and the table is restored. 30s is generous for a
	// 2s reconcile interval.
	testenv.AssertNftChainExists(t, testenv.DefaultNftTable, "prerouting_dnat", 30*time.Second)
}

// TestScenario_FailureInjection_OvsOfctlFailsOnce (#88 item 1, ovs-ofctl
// sub-case):
//
// Arm ovs-ofctl to fail every call in the next reconcile cycle, then start
// the agent with a local router + a FIP so a hairpin flow (cookie 0x998) is
// part of the desired state. The startup reconcile's ovs-ofctl calls all
// fail, so the hairpin flow never makes it onto br-ex. By the time the next
// periodic reconcile fires, the shim has exhausted its counter and chains
// through to real ovs-ofctl: EnsureOVSFlows + ReconcileOVSHairpinFlows
// install both the MAC-tweak (cookie 0x999) and hairpin flows.
//
// EnsureOVSFlows uses ovs-vsctl (not shimmed) for discovery, so the
// patchPort/ofport cache populates regardless of ovs-ofctl state — the
// recovery on cycle two does not need to re-run discovery.
func TestScenario_FailureInjection_OvsOfctlFailsOnce(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "fiofctl",
		LRPMAC:      "fa:16:3e:aa:00:01",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const fipA = "198.51.100.55"
	testenv.AddFIP(t, ctx, nb, router, fipA, "10.0.0.55")

	// failCount must exceed the ovs-ofctl call count in a single reconcile.
	// Since #241 a cycle costs at most a dump, a bundle probe and one
	// batched add per plane (2 planes × 3 = 6); a cycle whose dump already
	// fails costs just the two dumps, so an armed shim is consumed over
	// several cycles rather than one. Setting failCount to 10 keeps the
	// window above a single healthy cycle either way.
	shim := testenv.WithFailingTool(t, "ovs-ofctl", 10)
	cfg := testenv.FastDefaults()
	cfg.ExtraEnv = append(cfg.ExtraEnv, shim.Env())
	shim.Arm()

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Hairpin flow with the router MAC must appear once the shim
	// disarms — by then EnsureOVSFlows + ReconcileOVSHairpinFlows have
	// run successfully on cycle two.
	testenv.AssertOVSFlowMatches(t, "0x998",
		func(line string) bool {
			return strings.Contains(line, "nw_dst="+fipA) && strings.Contains(line, "mod_dl_dst:"+router.LRPMAC)
		}, 25*time.Second, "hairpin flow for FIP A → router MAC after recovery")

	logs := a.LogTail(100000)
	if !strings.Contains(logs, "test shim: forced failure of ovs-ofctl") {
		t.Errorf("expected forced-failure marker for ovs-ofctl in agent log; tail:\n%s", a.LogTail(40))
	}
}
