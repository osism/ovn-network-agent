//go:build integration

package integration

import (
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_OVNDatabasePauseResume (#64 scenario 1):
//
// SIGSTOP every ovsdb-server process for ~10s, then SIGCONT. The contract
// being pinned here is the agent's resilience against a transient OVN
// disconnect:
//
//   - the agent does NOT exit during the stall
//   - the kernel route installed before the pause is still present after
//     resume, and the agent process is still alive
//
// On the Ubuntu harness, ovn-ctl runs the NB ovsdb-server, the SB
// ovsdb-server, and ovn-northd under the single ovn-central systemd unit
// (see TestScenario_DrainStuckNBWrite for the long-form discussion). There
// is no clean way to pause only the NB server, so PauseOVNDatabases pauses
// both — a strictly harder version of the scenario described in the issue,
// since the agent now has zero OVN visibility instead of one DB.
//
// The pause window (10s) is intentionally shorter than libovsdb's 30s
// inactivity probe: this scenario covers the "stall and recover" path,
// not the "TCP reconnect" path. Reconnection through a longer outage —
// with ovn_connection_state asserted to drop and recover — is covered by
// TestScenario_OVNReconnectAfterLongOutage below.
func TestScenario_OVNDatabasePauseResume(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "reconnect",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	const fip = "198.51.100.66"
	testenv.AddFIP(t, ctx, nb, router, fip, "10.0.0.66")

	cfg := testenv.Defaults()
	cfg.ReconcileInterval = "2s"
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	// Pre-condition: the agent has installed the kernel route, so the
	// post-resume "still there" check has signal (rather than racing the
	// initial install against the pause window).
	testenv.AssertKernelRoute(t, fip, 15*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited before pause; setup is broken (last logs:\n%s)", a.LogTail(40))
	}

	// Pause both ovsdb-server processes for 10s. The helper sleeps
	// synchronously and SIGCONTs on return; a t.Cleanup inside the helper
	// guarantees resume even if the test goroutine fails mid-pause.
	testenv.PauseOVNDatabases(t, 10*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited during DB pause — resilience contract violated (last logs:\n%s)",
			a.LogTail(60))
	}

	// After resume, the kernel route must still be present. The agent
	// reads desired state from its libovsdb cache, which is unchanged
	// across the pause, and never tore the route down. AssertKernelRoute
	// polls so any reconcile tick that started mid-pause and only
	// completed after SIGCONT does not race the assertion.
	testenv.AssertKernelRoute(t, fip, 15*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited after DB resume — clean reconnect contract violated (last logs:\n%s)",
			a.LogTail(60))
	}
}

// TestScenario_OVNReconnectAfterLongOutage (#160):
//
// TestScenario_OVNDatabasePauseResume keeps its outage under libovsdb's 30s
// inactivity probe, so it never exercises a real disconnect/reconnect. This
// scenario holds the outage past the probe (>=45s), so libovsdb actually tears
// the connection down and re-establishes it. It pins the full reconnect
// surface a long-running daemon must survive:
//
//   - ovn_connection_state drops to 0 for both DBs — the proof the inactivity
//     probe fired and libovsdb disconnected rather than merely stalling — and
//     recovers to 1 after resume,
//   - /readyz tracks that transition (200 -> 503 -> 200), proving it
//     distinguishes an alive-but-disconnected agent from a functional one,
//   - the agent stays alive across the whole outage,
//   - the FIP route installed before the outage survives it, and
//   - a NB change made AFTER reconnect still drives reconciliation, proving
//     the monitors were re-established and the cache repopulated.
//
// It is event-driven (assert on the observed metric / kernel route) rather
// than sleep-calibrated, with a single deterministic hold to the >30s floor,
// so it stays green and non-flaky. The agent's metrics endpoint is served by a
// goroutine independent of OVSDB, so it keeps answering scrapes throughout the
// outage.
func TestScenario_OVNReconnectAfterLongOutage(t *testing.T) {
	// 3-minute ceiling: the outage runs ~60s (the probe-driven disconnect
	// asserted below) and recovery is asserted with generous budgets, all
	// comfortably inside it.
	ctx, cancel, nb, sb := startScenarioWithTimeout(t, 3*time.Minute)
	defer cancel()

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "reconnectlong",
		LRPNetworks: []string{"198.51.100.11/24"},
	})
	const fip1 = "198.51.100.67"
	testenv.AddFIP(t, ctx, nb, router, fip1, "10.0.0.67")

	cfg := testenv.FastDefaults()
	addr := testenv.FreeLoopbackAddr(t)
	cfg.MetricsListen = addr
	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)

	assertConnState := func(db string, want float64, timeout time.Duration) {
		t.Helper()
		testenv.AssertMetricEventually(t, addr,
			"ovn_network_agent_ovn_connection_state",
			map[string]string{"database": db},
			func(v float64, present bool) bool { return present && v == want },
			timeout)
	}

	// Pre-condition: the route landed, both DB connections read as up, and
	// /readyz reports the agent functional.
	testenv.AssertKernelRoute(t, fip1, 15*time.Second)
	assertConnState("nb", 1, 10*time.Second)
	assertConnState("sb", 1, 10*time.Second)
	testenv.AssertHTTPStatusEventually(t, addr, "/readyz", 200, 10*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited before the outage; setup is broken (last logs:\n%s)", a.LogTail(40))
	}

	// Open the outage and hold it past the 30s inactivity probe.
	start := time.Now()
	resume := testenv.StopOVNDatabases(t)

	// Both gauges must drop to 0 — the proof the probe fired and libovsdb
	// disconnected. The drop takes up to ~60s from the last pre-outage
	// traffic: 30s of silence arm the probe, and the probe echo then gets
	// options.inactivityTimeout (another 30s) to time out against the
	// stopped server — WithInactivityCheck's second argument is the
	// reconnect dial timeout, not the echo timeout. 90s adds real margin.
	assertConnState("nb", 0, 90*time.Second)
	assertConnState("sb", 0, 90*time.Second)

	// With both DBs disconnected the agent is alive but not functional, so
	// /readyz must report 503 even though the process is up and /healthz still
	// answers 200 — the #165 acceptance criterion distinguishing process-alive
	// from agent-functional. Readiness flipped with the gauges above, so a
	// short timeout suffices.
	testenv.AssertHTTPStatusEventually(t, addr, "/readyz", 503, 10*time.Second)

	// Honour the >40s outage floor deterministically by holding the remainder
	// of 45s (the gauge-drop asserts above already consumed most of it).
	if held := time.Since(start); held < 45*time.Second {
		time.Sleep(45*time.Second - held)
	}

	if !a.Alive() {
		t.Fatalf("agent exited during the OVN outage — resilience contract violated (last logs:\n%s)",
			a.LogTail(60))
	}

	resume()

	// Recovery: both gauges climb back to 1, and /readyz returns to 200 once a
	// post-reconnect reconcile completes.
	assertConnState("nb", 1, 45*time.Second)
	assertConnState("sb", 1, 45*time.Second)
	testenv.AssertHTTPStatusEventually(t, addr, "/readyz", 200, 30*time.Second)

	// The pre-outage route survived the disconnect/reconnect.
	testenv.AssertKernelRoute(t, fip1, 15*time.Second)

	// A NB change made after reconnect still drives reconciliation — the
	// monitors were re-established and the cache repopulated, not left stale.
	const fip2 = "198.51.100.68"
	testenv.AddFIP(t, ctx, nb, router, fip2, "10.0.0.68")
	testenv.AssertKernelRoute(t, fip2, 30*time.Second)

	if !a.Alive() {
		t.Fatalf("agent exited after OVN recovery — clean reconnect contract violated (last logs:\n%s)",
			a.LogTail(60))
	}
}
