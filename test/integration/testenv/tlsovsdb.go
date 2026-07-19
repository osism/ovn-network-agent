//go:build integration

package testenv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TLSMaterial holds the PEM file paths of a self-signed CA plus a server and a
// client leaf certificate, all generated on the fly for a single test. The CA
// signs both leaves, so a client presenting ClientCert/ClientKey and trusting
// CACert can complete mutual TLS against a server configured with
// ServerCert/ServerKey/CACert. Used to stand up a pssl: ovsdb-server that
// requires (and the agent presents) a client certificate.
type TLSMaterial struct {
	CACert     string
	ServerCert string
	ServerKey  string
	ClientCert string
	ClientKey  string
}

// GenerateOVSDBTLS mints a private ECDSA P-256 CA and a server and client leaf
// signed by it, writes all five PEM files (CA cert, server cert/key, client
// cert/key) into t.TempDir() with 0600 perms, and returns their paths.
//
// The server leaf carries a 127.0.0.1 IP SAN and a "localhost" DNS SAN with
// ExtKeyUsageServerAuth so a Go client verifying the dialed host succeeds; the
// client leaf carries ExtKeyUsageClientAuth so an ovsdb-server started with
// --ca-cert accepts it. Certificates are valid from one hour ago to one hour
// from now, sidestepping clock skew without being long-lived.
func GenerateOVSDBTLS(t *testing.T) TLSMaterial {
	t.Helper()
	dir := t.TempDir()

	notBefore := time.Now().Add(-1 * time.Hour)
	notAfter := time.Now().Add(1 * time.Hour)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: "ovn-network-agent integration test CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	serverTmpl := &x509.Certificate{
		SerialNumber: randomSerial(t),
		Subject:      pkix.Name{CommonName: "ovsdb-server"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTmpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	clientTmpl := &x509.Certificate{
		SerialNumber: randomSerial(t),
		Subject:      pkix.Name{CommonName: "ovn-network-agent"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTmpl, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create client certificate: %v", err)
	}

	m := TLSMaterial{
		CACert:     filepath.Join(dir, "ca.pem"),
		ServerCert: filepath.Join(dir, "server-cert.pem"),
		ServerKey:  filepath.Join(dir, "server-key.pem"),
		ClientCert: filepath.Join(dir, "client-cert.pem"),
		ClientKey:  filepath.Join(dir, "client-key.pem"),
	}
	writeCertPEM(t, m.CACert, caDER)
	writeCertPEM(t, m.ServerCert, serverDER)
	writeKeyPEM(t, m.ServerKey, serverKey)
	writeCertPEM(t, m.ClientCert, clientDER)
	writeKeyPEM(t, m.ClientKey, clientKey)
	return m
}

// StartTLSOVSDBServer creates a fresh ovsdb database named <name> from
// schemaPath, launches an ovsdb-server that serves it over pssl: on a free
// loopback port using m's server certificate and requiring a client
// certificate (via --ca-cert), and returns the "ssl:127.0.0.1:<port>" remote
// string the agent should dial. The server process is killed on test cleanup.
//
// Readiness is polled by completing a mutual-TLS handshake with m's client
// keypair, so a returned remote is guaranteed connectable and — because the
// server demands a client certificate — the successful handshake already
// proves mutual TLS against the private CA. On failure the server's combined
// output is dumped to the test log to make a handshake or startup problem
// obvious.
func StartTLSOVSDBServer(t *testing.T, name, schemaPath string, m TLSMaterial) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, name+".db")

	if out, err := exec.Command("ovsdb-tool", "create", dbPath, schemaPath).CombinedOutput(); err != nil {
		t.Fatalf("ovsdb-tool create %s from %s: %v (output: %s)",
			dbPath, schemaPath, err, strings.TrimSpace(string(out)))
	}

	addr := FreeLoopbackAddr(t)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port %q: %v", addr, err)
	}

	logPath := filepath.Join(dir, name+"-ovsdb-server.log")
	logF, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("create ovsdb-server log %s: %v", logPath, err)
	}

	cmd := exec.Command("ovsdb-server", dbPath,
		"--remote=pssl:"+port+":127.0.0.1",
		"--certificate="+m.ServerCert,
		"--private-key="+m.ServerKey,
		"--ca-cert="+m.CACert,
		"--no-chdir",
	)
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		_ = logF.Close()
		t.Fatalf("start ovsdb-server for %s: %v", name, err)
	}

	// Dump the server log if the test fails. Registered before the kill
	// cleanup so it runs first (LIFO) and reads the log while the process is
	// still up; the content comes from disk, so ordering is not load-bearing.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		out, rerr := os.ReadFile(logPath)
		if rerr != nil {
			t.Logf("ovsdb-server %s: read log %s: %v", name, logPath, rerr)
			return
		}
		t.Logf("ovsdb-server %s log:\n%s", name, strings.TrimRight(string(out), "\n"))
	})
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logF.Close()
	})

	tlsCfg := clientTLSConfig(t, m)
	dialAddr := "127.0.0.1:" + port
	Eventually(t, func() bool {
		conn, derr := tls.Dial("tcp", dialAddr, tlsCfg)
		if derr != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 10*time.Second, 200*time.Millisecond,
		fmt.Sprintf("ovsdb-server %s did not accept a mutual-TLS connection on %s", name, dialAddr))

	return "ssl:127.0.0.1:" + port
}

// clientTLSConfig builds a tls.Config that trusts m's CA and presents m's
// client keypair, used to probe StartTLSOVSDBServer readiness with a real
// mutual-TLS handshake.
func clientTLSConfig(t *testing.T, m TLSMaterial) *tls.Config {
	t.Helper()
	caPEM, err := os.ReadFile(m.CACert)
	if err != nil {
		t.Fatalf("read CA %s: %v", m.CACert, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("no PEM certificates in CA %s", m.CACert)
	}
	cert, err := tls.LoadX509KeyPair(m.ClientCert, m.ClientKey)
	if err != nil {
		t.Fatalf("load client keypair: %v", err)
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS12,
	}
}

// writeCertPEM writes a DER certificate to path as a CERTIFICATE PEM block.
func writeCertPEM(t *testing.T, path string, der []byte) {
	t.Helper()
	buf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write certificate %s: %v", path, err)
	}
}

// writeKeyPEM marshals key with x509.MarshalECPrivateKey and writes it to path
// as an EC PRIVATE KEY PEM block.
func writeKeyPEM(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal EC private key: %v", err)
	}
	buf := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write private key %s: %v", path, err)
	}
}

// randomSerial returns a random 128-bit certificate serial number.
func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	return n
}
