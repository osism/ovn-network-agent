//go:build integration

package testenv

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// patch port names used by EnsureBridgePatchPort. Long enough to be visibly
// "from the test harness" in any leftover diagnostic output.
const (
	testenvPatchOnBrEx  = "patch-testenv-brex"
	testenvPatchOnBrInt = "patch-testenv-brint"
)

// EnsureBridgePatchPort creates a patch-port pair between br-int and br-ex so
// the agent's discoverPatchPort can find a type=patch port to bind its OVS
// MAC-tweak / hairpin flows to. Idempotent — re-running is a no-op.
//
// Without this, scenarios that bypass ovn-northd never have a patch port on
// br-ex (because ovn-controller only creates them in response to logical
// switch state in SB). Tests that assert OVS flows depend on it.
func EnsureBridgePatchPort(t *testing.T) {
	t.Helper()

	addPort := func(bridge, port, peer string) {
		// `--may-exist add-port` is idempotent for the port row but not for
		// the interface options; set them explicitly each time.
		out, err := exec.Command("ovs-vsctl",
			"--may-exist", "add-port", bridge, port, "--",
			"set", "Interface", port, "type=patch", "options:peer="+peer,
		).CombinedOutput()
		if err != nil {
			t.Fatalf("create patch port %s on %s: %v (output: %s)",
				port, bridge, err, strings.TrimSpace(string(out)))
		}
	}
	addPort(DefaultBridgeDev, testenvPatchOnBrEx, testenvPatchOnBrInt)
	addPort("br-int", testenvPatchOnBrInt, testenvPatchOnBrEx)
}

// EnsureSegmentPatchPort creates a patch-port pair between br-ex and br-int
// whose br-ex Port row carries external_ids:ovn-localnet-port=<localnetPort>,
// mirroring what ovn-controller stamps on the provider-bridge patch port of
// each localnet network. The agent's per-segment discovery maps the localnet
// port name to this patch port. Idempotent; the ports are removed at test
// cleanup so leftover tagged ports cannot skew single-network scenarios'
// first-patch-port fallback.
func EnsureSegmentPatchPort(t *testing.T, localnetPort string) {
	t.Helper()

	brexPort := "patch-" + localnetPort + "-to-br-int"
	brintPort := "patch-br-int-to-" + localnetPort

	addPort := func(bridge, port, peer string) {
		out, err := exec.Command("ovs-vsctl",
			"--may-exist", "add-port", bridge, port, "--",
			"set", "Interface", port, "type=patch", "options:peer="+peer,
		).CombinedOutput()
		if err != nil {
			t.Fatalf("create patch port %s on %s: %v (output: %s)",
				port, bridge, err, strings.TrimSpace(string(out)))
		}
	}
	addPort(DefaultBridgeDev, brexPort, brintPort)
	addPort("br-int", brintPort, brexPort)

	if out, err := exec.Command("ovs-vsctl", "set", "Port", brexPort,
		"external_ids:ovn-localnet-port="+localnetPort).CombinedOutput(); err != nil {
		t.Fatalf("set ovn-localnet-port on %s: %v (output: %s)",
			brexPort, err, strings.TrimSpace(string(out)))
	}

	t.Cleanup(func() {
		_ = exec.Command("ovs-vsctl", "--if-exists", "del-port", DefaultBridgeDev, brexPort).Run()
		_ = exec.Command("ovs-vsctl", "--if-exists", "del-port", "br-int", brintPort).Run()
	})
}

// SegmentPatchOfport returns the OpenFlow port number of the br-ex patch port
// created by EnsureSegmentPatchPort for the given localnet port name.
func SegmentPatchOfport(t *testing.T, localnetPort string) string {
	t.Helper()
	port := "patch-" + localnetPort + "-to-br-int"
	out, err := exec.Command("ovs-vsctl", "get", "Interface", port, "ofport").CombinedOutput()
	if err != nil {
		t.Fatalf("get ofport for %s: %v (output: %s)", port, err, strings.TrimSpace(string(out)))
	}
	ofport := strings.TrimSpace(string(out))
	if ofport == "" || ofport == "-1" {
		t.Fatalf("invalid ofport %q for %s", ofport, port)
	}
	return ofport
}

// PauseOVNNorthd suspends the ovn-northd processing loop for the duration of
// the test, registering a Cleanup that resumes it.
//
// Scenario tests that drive SB Port_Binding entries directly need ovn-northd
// out of the picture: northd treats SB as a pure projection of NB and would
// otherwise garbage-collect manually-inserted Port_Binding/Datapath_Binding
// rows within seconds.
//
// We use OVN's built-in `ovn-appctl -t ovn-northd pause/resume` rather than
// stopping the systemd unit. On Ubuntu the `ovn-northd.service` unit is a
// thin wrapper around `ovn-ctl start_northd`/`stop_northd`, which manages
// the NB ovsdb-server, SB ovsdb-server, AND the northd daemon together —
// so a `systemctl stop ovn-northd` would also take down the databases the
// test needs to talk to.
func PauseOVNNorthd(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("ovn-appctl"); err != nil {
		t.Skipf("ovn-appctl not found in PATH: %v", err)
	}

	// `ovn-appctl -t ovn-northd pause` is idempotent; re-running it on an
	// already-paused daemon returns success.
	t.Logf("pausing ovn-northd processing for the duration of the test")
	if out, err := exec.Command("ovn-appctl", "-t", "ovn-northd", "pause").CombinedOutput(); err != nil {
		// If northd is not running at all, there is nothing to pause and
		// nothing to garbage-collect SB rows we insert — treat it as a
		// successful no-op rather than failing the test.
		txt := strings.TrimSpace(string(out))
		if isNorthdUnreachable(txt) {
			t.Logf("PauseOVNNorthd: ovn-northd not reachable via appctl (%s); assuming not running", txt)
			return
		}
		t.Fatalf("pause ovn-northd: %v (output: %s)", err, txt)
	}

	t.Cleanup(func() {
		if out, err := exec.Command("ovn-appctl", "-t", "ovn-northd", "resume").CombinedOutput(); err != nil {
			t.Logf("resume ovn-northd after test: %v (output: %s)", err, strings.TrimSpace(string(out)))
		}
	})
}

// isNorthdUnreachable distinguishes "ovn-northd is not running / its control
// socket does not exist" from "the pause command itself failed". The former
// is harmless for tests; the latter must surface as a failure.
func isNorthdUnreachable(out string) bool {
	out = strings.ToLower(out)
	return strings.Contains(out, "no such file") ||
		strings.Contains(out, "connection refused") ||
		strings.Contains(out, "cannot connect")
}

// PauseOVNDatabases suspends every ovsdb-server process via SIGSTOP for the
// duration d, then SIGCONTs them. This simulates a transient OVN disconnect:
// the TCP socket stays open but reads stall, which is the same shape a
// production-side network blip or a paused remote presents to libovsdb.
//
// Ubuntu's ovn-ctl runs the NB ovsdb-server, the SB ovsdb-server, and
// ovn-northd under the single ovn-central unit. There is no clean way to
// pause only the NB server without taking the SB server down with it (see
// the comment on TestScenario_DrainStuckNBWrite in scenario_drain_edges_test.go),
// so this helper pauses both. The contract being tested by callers is "the
// agent does NOT exit when its only DB endpoint becomes unresponsive" —
// pausing both DBs is a strictly harder version of the scenario than the
// single-DB pause described in #64, so the test still proves the contract.
//
// The function blocks for d while the DBs are paused. Tests should keep d
// well under libovsdb's inactivity timeout (production: 30s) so reconnect
// logic does not kick in mid-pause — that is a separate scenario.
//
// PauseOVNDatabases is a synchronous primitive on purpose: scenarios that
// run direct OVSDB transactions against the test driver's NB/SB clients
// would also stall during the pause, so all setup must happen before this
// call and all post-resume assertions after it.
func PauseOVNDatabases(t *testing.T, d time.Duration) {
	t.Helper()

	if _, err := exec.LookPath("pkill"); err != nil {
		t.Skipf("pkill not found in PATH: %v", err)
	}

	t.Logf("SIGSTOP all ovsdb-server processes for %s", d)
	if out, err := exec.Command("pkill", "-STOP", "-x", "ovsdb-server").CombinedOutput(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			t.Skip("PauseOVNDatabases: no ovsdb-server process found; cannot exercise reconnect contract")
			return
		}
		t.Fatalf("SIGSTOP ovsdb-server: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	// Resume is registered as a cleanup before we sleep, so a test failure
	// or panic inside the sleep window does not leave ovsdb-server stopped.
	resumed := false
	t.Cleanup(func() {
		if resumed {
			return
		}
		if out, err := exec.Command("pkill", "-CONT", "-x", "ovsdb-server").CombinedOutput(); err != nil {
			t.Logf("cleanup SIGCONT ovsdb-server: %v (output: %s)", err, strings.TrimSpace(string(out)))
		}
	})

	time.Sleep(d)

	t.Logf("SIGCONT all ovsdb-server processes (resume)")
	if out, err := exec.Command("pkill", "-CONT", "-x", "ovsdb-server").CombinedOutput(); err != nil {
		t.Fatalf("SIGCONT ovsdb-server: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}
	resumed = true
}

// PauseOVNController suspends ovn-controller's main processing loop for the
// duration of the test, registering a Cleanup that resumes it.
//
// Scenario tests that drive SB Port_Binding entries directly need
// ovn-controller out of the picture too: ovn-controller continuously
// reclaims chassisredirect Port_Bindings based on SB Gateway_Chassis
// records, and with northd paused those records don't exist. Left to its
// own devices, ovn-controller decides our hand-set Port_Binding.chassis
// shouldn't be bound here and clears the field within milliseconds —
// which is exactly the column the agent's local-router detection reads.
//
// Unlike ovn-northd, ovn-controller has no `pause`/`resume` appctl, so we
// suspend it at the process level with SIGSTOP and resume it with SIGCONT
// at test cleanup. The process keeps its OVSDB connections (just doesn't
// drive them) and the local Chassis/Encap rows it registered remain
// intact.
func PauseOVNController(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("pkill"); err != nil {
		t.Skipf("pkill not found in PATH: %v", err)
	}

	t.Logf("suspending ovn-controller (SIGSTOP) for the duration of the test")
	if out, err := exec.Command("pkill", "-STOP", "-x", "ovn-controller").CombinedOutput(); err != nil {
		// pkill exits 1 when no processes matched. That's fine — there's
		// nothing to stop, so nothing will reclaim our Port_Bindings.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			t.Logf("PauseOVNController: no ovn-controller process found; nothing to suspend")
			return
		}
		t.Fatalf("SIGSTOP ovn-controller: %v (output: %s)", err, strings.TrimSpace(string(out)))
	}

	t.Cleanup(func() {
		if out, err := exec.Command("pkill", "-CONT", "-x", "ovn-controller").CombinedOutput(); err != nil {
			t.Logf("SIGCONT ovn-controller after test: %v (output: %s)", err, strings.TrimSpace(string(out)))
		}
	})
}
