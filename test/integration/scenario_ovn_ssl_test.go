//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_OVNSSLMutualTLS (#164):
//
// Provisions a dedicated, empty NB and SB ovsdb-server from the checked-in
// schemas, each listening on a pssl: endpoint with a per-test private CA. The
// servers are started with --ca-cert, so they REQUIRE a client certificate;
// the agent is pointed at those ssl: remotes and handed the matching
// ovn_ssl_ca/ovn_ssl_cert/ovn_ssl_key, so its reaching "agent running" — which
// only fires after Connect completes against both databases — proves it
// negotiated mutual TLS with the private CA on the NB and SB connections. The
// ovn_connection_state gauge for each database is then asserted to settle at 1
// as an independent confirmation the sessions are live.
//
// This runs against fresh standalone databases rather than the harness's
// ovn-central instance so the scenario owns the TLS listeners outright and does
// not disturb the plaintext 6641/6642 servers the other scenarios rely on.
func TestScenario_OVNSSLMutualTLS(t *testing.T) {
	testenv.Setup(t)

	m := testenv.GenerateOVSDBTLS(t)
	nbRemote := testenv.StartTLSOVSDBServer(t, "nb", "../../schemas/ovn-nb.ovsschema", m)
	sbRemote := testenv.StartTLSOVSDBServer(t, "sb", "../../schemas/ovn-sb.ovsschema", m)

	cfg := testenv.Defaults()
	cfg.OVNNBRemote = nbRemote
	cfg.OVNSBRemote = sbRemote
	cfg.OVNSSLCA = m.CACert
	cfg.OVNSSLCert = m.ClientCert
	cfg.OVNSSLKey = m.ClientKey
	on := true
	cfg.DryRun = &on
	off := false
	cfg.VethLeakEnabled = &off
	cfg.MetricsListen = testenv.FreeLoopbackAddr(t)

	// WithAgent's WaitReady keys on "agent running", which the agent only logs
	// after both TLS Connects succeed and the initial reconcile completes.
	a := testenv.WithAgent(t, cfg)
	_ = a

	for _, db := range []string{"nb", "sb"} {
		testenv.AssertMetricEventually(t, cfg.MetricsListen,
			"ovn_network_agent_ovn_connection_state",
			map[string]string{"database": db},
			func(v float64, present bool) bool { return present && v == 1 },
			10*time.Second)
	}
}

// TestScenario_OVNSSLRejectsUntrustedServerCert (#164):
//
// The refusal direction of the scenario above. Two independent private CAs are
// minted: the ovsdb-servers are given a certificate from the first, the agent
// is told to trust only the second. Every other input stays valid — the agent's
// own client keypair chains to the CA it was handed, and both remotes are
// reachable — so the only reason the session can fail is that the agent
// verified the server certificate and found no path to its configured CA.
//
// Without this, the suite would pass just as happily with InsecureSkipVerify
// set or with the tls.Config dropped on the way to libovsdb: the mutual-TLS
// scenario only proves a matching CA works, never that a mismatched one is
// refused. The agent is expected to stay up retrying (connectWithRetry loops
// forever), so the assertion is that the x509 failure reaches the log and
// "agent running" never does.
func TestScenario_OVNSSLRejectsUntrustedServerCert(t *testing.T) {
	testenv.Setup(t)

	served := testenv.GenerateOVSDBTLS(t)  // CA A — signs the ovsdb-servers
	trusted := testenv.GenerateOVSDBTLS(t) // CA B — all the agent trusts

	nbRemote := testenv.StartTLSOVSDBServer(t, "nb", "../../schemas/ovn-nb.ovsschema", served)
	sbRemote := testenv.StartTLSOVSDBServer(t, "sb", "../../schemas/ovn-sb.ovsschema", served)

	cfg := testenv.Defaults()
	cfg.OVNNBRemote = nbRemote
	cfg.OVNSBRemote = sbRemote
	cfg.OVNSSLCA = trusted.CACert
	cfg.OVNSSLCert = trusted.ClientCert
	cfg.OVNSSLKey = trusted.ClientKey
	on := true
	cfg.DryRun = &on
	off := false
	cfg.VethLeakEnabled = &off

	a := testenv.RunAgent(t, cfg)
	t.Cleanup(func() {
		if err := a.Stop(15 * time.Second); err != nil {
			t.Errorf("agent stop: %v", err)
		}
	})

	// connectWithRetry logs "failed to connect to OVN, retrying" with the dial
	// error; Go's verifier reports an unknown signer as this x509 error.
	testenv.Eventually(t, func() bool {
		return strings.Contains(a.LogTail(200), "certificate signed by unknown authority")
	}, 30*time.Second, 200*time.Millisecond,
		"agent should refuse a server certificate signed by an untrusted CA")

	if tail := a.LogTail(200); strings.Contains(tail, "agent running") {
		t.Fatalf("agent reached \"agent running\" against an untrusted server certificate; last logs:\n%s", tail)
	}
}

// TestScenario_OVNSSLBadCAPathFailsStartup (#164):
//
// A non-existent ovn-ssl-ca path must fail the config load at startup rather
// than deferring to the first OVN dial. The agent is launched (RunAgent, not
// WithAgent, because it is expected to exit) and asserted to terminate, and its
// stderr must name the offending "ovn-ssl-ca" option — locking the acceptance
// criterion that misconfigured cert paths fail at startup with an actionable
// message, not at first reconnect.
func TestScenario_OVNSSLBadCAPathFailsStartup(t *testing.T) {
	testenv.Setup(t)

	// The default tcp: remotes are irrelevant: config validation reads the CA
	// path and fails before any connection is attempted.
	cfg := testenv.Defaults()
	cfg.OVNSSLCA = "/nonexistent/ca.pem"
	on := true
	cfg.DryRun = &on
	off := false
	cfg.VethLeakEnabled = &off

	a := testenv.RunAgent(t, cfg)

	// Alive() is the reliable exit signal: WaitReady's readiness channel has a
	// small race between the stderr EOF and cmd.Wait returning.
	testenv.Eventually(t, func() bool { return !a.Alive() },
		10*time.Second, 100*time.Millisecond,
		"agent should exit on bad ovn-ssl-ca path")

	if tail := a.LogTail(50); !strings.Contains(tail, "ovn-ssl-ca") {
		t.Fatalf("startup error should name the ovn-ssl-ca option; last logs:\n%s", tail)
	}
}
