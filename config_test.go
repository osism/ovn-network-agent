package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// applyYAMLConfig applies a config-file fragment to cfg through the option
// registry — the file layer exactly as loadConfig drives it. Tests express the
// file layer as the YAML an operator would actually write, rather than as a
// mirror struct that no longer exists.
func applyYAMLConfig(t *testing.T, cfg *Config, content string) error {
	t.Helper()
	var doc configDoc
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("parse test yaml: %v", err)
	}
	return applyFileConfig(cfg, doc)
}

// readFileConfig writes content to a temp config file and returns the Config
// produced by the file layer, exercising readConfigFile + applyFileConfig.
func readFileConfig(t *testing.T, content string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	doc, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := applyFileConfig(&cfg, doc); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// fullModeArgs prepends the OVN remote flags required for full mode to the
// given extra flags. Tests that exercise unrelated config fields use it so
// the operating-mode validation matrix (validateMode) does not reject an
// otherwise-incomplete config.
func fullModeArgs(extra ...string) []string {
	return append([]string{
		"--ovn-sb-remote", "tcp:10.0.0.1:6642",
		"--ovn-nb-remote", "tcp:10.0.0.1:6641",
	}, extra...)
}

func TestReadConfigFile(t *testing.T) {
	cfg, err := readFileConfig(t, `
ovn_sb_remote: "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
bridge_dev: "br-provider"
vrf_name: "vrf-test"
veth_nexthop: "169.254.0.2"
network_cidr: "192.0.2.0/24"
gateway_port: "cr-lrp-abc123"
reconcile_interval: "30s"
log_level: "debug"
cleanup_on_shutdown: false
`)
	if err != nil {
		t.Fatalf("readFileConfig() error: %v", err)
	}

	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642")
	}
	if cfg.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", cfg.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
	if cfg.VRFName != "vrf-test" {
		t.Errorf("VRFName = %q, want %q", cfg.VRFName, "vrf-test")
	}
	if cfg.VethNexthop != "169.254.0.2" {
		t.Errorf("VethNexthop = %q, want %q", cfg.VethNexthop, "169.254.0.2")
	}
	// A scalar network_cidr is accepted as a one-element list.
	if len(cfg.NetworkCIDRs) != 1 || cfg.NetworkCIDRs[0] != "192.0.2.0/24" {
		t.Errorf("NetworkCIDRs = %v, want [192.0.2.0/24]", cfg.NetworkCIDRs)
	}
	if cfg.GatewayPort != "cr-lrp-abc123" {
		t.Errorf("GatewayPort = %q, want %q", cfg.GatewayPort, "cr-lrp-abc123")
	}
	if cfg.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 30*time.Second)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown = true, want false")
	}
}

func TestReadConfigFileNotFound(t *testing.T) {
	_, err := readConfigFile("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestReadConfigFileInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	_, err := readConfigFile(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoadConfigWarnsOnUnknownFileKey(t *testing.T) {
	t.Run("typo warns but is accepted", func(t *testing.T) {
		buf := captureSlog(t)
		// "drain_on_shutdow" is a typo of "drain_on_shutdown"; the
		// unknown key must be flagged, and the default (true) applied.
		content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
veth_leak_enabled: false
drain_on_shutdow: false
`
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write test config: %v", err)
		}

		cfg, err := loadConfig([]string{"--config", path})
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if !cfg.DrainOnShutdown {
			t.Error("DrainOnShutdown should keep its default (true) when the key is a typo")
		}
		if !strings.Contains(buf.String(), "drain_on_shutdow") {
			t.Errorf("expected a warning naming drain_on_shutdow, got: %q", buf.String())
		}
	})

	t.Run("valid file produces no unknown-key warning", func(t *testing.T) {
		buf := captureSlog(t)
		content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
veth_leak_enabled: false
drain_on_shutdown: false
`
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write test config: %v", err)
		}
		if _, err := loadConfig([]string{"--config", path}); err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if strings.Contains(buf.String(), "unknown keys") {
			t.Errorf("unexpected unknown-key warning for a valid file: %q", buf.String())
		}
	})

	t.Run("comment-only file produces no unknown-key warning", func(t *testing.T) {
		buf := captureSlog(t)
		// Only comments and blank lines: the strict probe decode returns
		// io.EOF, which must not be mistaken for an unknown key.
		content := "# just a comment\n\n"
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write test config: %v", err)
		}
		if _, err := readConfigFile(path); err != nil {
			t.Fatalf("readConfigFile() error: %v", err)
		}
		if strings.Contains(buf.String(), "unknown keys") {
			t.Errorf("unexpected unknown-key warning for a comment-only file: %q", buf.String())
		}
	})
}

func TestReadConfigFileNetworkCIDRList(t *testing.T) {
	cfg, err := readFileConfig(t, `
network_cidr:
  - "192.0.2.0/24"
  - "198.51.100.0/24"
`)
	if err != nil {
		t.Fatalf("readFileConfig() error: %v", err)
	}

	if len(cfg.NetworkCIDRs) != 2 {
		t.Fatalf("NetworkCIDRs length = %d, want 2", len(cfg.NetworkCIDRs))
	}
	if cfg.NetworkCIDRs[0] != "192.0.2.0/24" || cfg.NetworkCIDRs[1] != "198.51.100.0/24" {
		t.Errorf("NetworkCIDRs = %v, want [192.0.2.0/24 198.51.100.0/24]", cfg.NetworkCIDRs)
	}
}

// StringOrSlice accepts either a scalar string or a YAML sequence. A mapping
// node (the third YAML container kind) is malformed for this type and must
// surface as a descriptive decode error — not a panic, not a silent empty
// value that would make a typo'd `network_cidr: {foo: bar}` look like an
// empty filter list and quietly disable network filtering.
func TestStringOrSliceUnmarshalRejectsMappingInput(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "inline mapping",
			yaml: `network_cidr: {foo: bar}` + "\n",
		},
		{
			name: "block mapping",
			yaml: "network_cidr:\n  foo: bar\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Must not panic on a mapping node — the option decodes the value
			// into a StringOrSlice, and yaml.v3 returns an error from Decode
			// rather than panicking. Catch any panic explicitly so a future
			// refactor that drops the safe Decode path is flagged here.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("UnmarshalYAML panicked on mapping input: %v", r)
				}
			}()

			cfg, err := readFileConfig(t, tt.yaml)
			if err == nil {
				t.Fatalf("expected decode error for mapping input, got success with NetworkCIDRs=%v", cfg.NetworkCIDRs)
			}
			if !strings.Contains(err.Error(), "network_cidr") {
				t.Errorf("error %q does not name network_cidr", err)
			}
		})
	}
}

func TestApplyEnvConfigMultipleCIDRs(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_NETWORK_CIDR", "10.0.0.0/24,172.16.0.0/12")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}

	if len(cfg.NetworkCIDRs) != 2 {
		t.Fatalf("NetworkCIDRs length = %d, want 2", len(cfg.NetworkCIDRs))
	}
	if cfg.NetworkCIDRs[0] != "10.0.0.0/24" || cfg.NetworkCIDRs[1] != "172.16.0.0/12" {
		t.Errorf("NetworkCIDRs = %v, want [10.0.0.0/24 172.16.0.0/12]", cfg.NetworkCIDRs)
	}
}

func TestApplyFileConfig(t *testing.T) {
	cfg := Config{
		BridgeDev:         "br-ex",
		VRFName:           "vrf-provider",
		VethNexthop:       "169.254.0.1",
		ReconcileInterval: 60 * time.Second,
		LogLevel:          "info",
	}

	if err := applyYAMLConfig(t, &cfg, `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
ovn_ssl_ca: "/etc/ovn/ca.pem"
ovn_ssl_cert: "/etc/ovn/cert.pem"
ovn_ssl_key: "/etc/ovn/key.pem"
bridge_dev: "br-provider"
reconcile_interval: "30s"
`); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}

	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
	if cfg.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", cfg.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if cfg.OVNSSLCA != "/etc/ovn/ca.pem" {
		t.Errorf("OVNSSLCA = %q, want %q", cfg.OVNSSLCA, "/etc/ovn/ca.pem")
	}
	if cfg.OVNSSLCert != "/etc/ovn/cert.pem" {
		t.Errorf("OVNSSLCert = %q, want %q", cfg.OVNSSLCert, "/etc/ovn/cert.pem")
	}
	if cfg.OVNSSLKey != "/etc/ovn/key.pem" {
		t.Errorf("OVNSSLKey = %q, want %q", cfg.OVNSSLKey, "/etc/ovn/key.pem")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
	// Keys absent from the file keep their prior value.
	if cfg.VRFName != "vrf-provider" {
		t.Errorf("VRFName = %q, want %q", cfg.VRFName, "vrf-provider")
	}
	if cfg.VethNexthop != "169.254.0.1" {
		t.Errorf("VethNexthop = %q, want %q", cfg.VethNexthop, "169.254.0.1")
	}
	if cfg.ReconcileInterval != 30*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 30*time.Second)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
}

func TestApplyFileConfigEmptyFieldsNoOverride(t *testing.T) {
	cfg := Config{
		BridgeDev: "br-ex",
		LogLevel:  "info",
	}

	// An empty file must not clobber anything.
	if err := applyYAMLConfig(t, &cfg, "\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}

	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want %q (should not be overridden)", cfg.BridgeDev, "br-ex")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q (should not be overridden)", cfg.LogLevel, "info")
	}

	// An explicitly empty string must likewise not clobber a real value:
	// that is the historical `if fc.X != ""` contract.
	if err := applyYAMLConfig(t, &cfg, `bridge_dev: ""`+"\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want br-ex (empty string must not override)", cfg.BridgeDev)
	}
}

func TestApplyEnvConfig(t *testing.T) {
	cfg := Config{
		BridgeDev: "br-ex",
		LogLevel:  "info",
	}

	t.Setenv("OVN_NETWORK_OVN_SB_REMOTE", "tcp:10.0.0.99:6642")
	t.Setenv("OVN_NETWORK_OVN_SSL_CA", "/etc/ovn/ca.pem")
	t.Setenv("OVN_NETWORK_OVN_SSL_CERT", "/etc/ovn/cert.pem")
	t.Setenv("OVN_NETWORK_OVN_SSL_KEY", "/etc/ovn/key.pem")
	t.Setenv("OVN_NETWORK_LOG_LEVEL", "debug")
	t.Setenv("OVN_NETWORK_RECONCILE_INTERVAL", "5m")
	t.Setenv("OVN_NETWORK_NETWORK_CIDR", "10.0.0.0/24")
	t.Setenv("OVN_NETWORK_GATEWAY_PORT", "cr-lrp-test")

	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}

	if cfg.OVNSBRemote != "tcp:10.0.0.99:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.99:6642")
	}
	if cfg.OVNSSLCA != "/etc/ovn/ca.pem" {
		t.Errorf("OVNSSLCA = %q, want %q", cfg.OVNSSLCA, "/etc/ovn/ca.pem")
	}
	if cfg.OVNSSLCert != "/etc/ovn/cert.pem" {
		t.Errorf("OVNSSLCert = %q, want %q", cfg.OVNSSLCert, "/etc/ovn/cert.pem")
	}
	if cfg.OVNSSLKey != "/etc/ovn/key.pem" {
		t.Errorf("OVNSSLKey = %q, want %q", cfg.OVNSSLKey, "/etc/ovn/key.pem")
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "debug")
	}
	if cfg.ReconcileInterval != 5*time.Minute {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 5*time.Minute)
	}
	if len(cfg.NetworkCIDRs) != 1 || cfg.NetworkCIDRs[0] != "10.0.0.0/24" {
		t.Errorf("NetworkCIDRs = %v, want [10.0.0.0/24]", cfg.NetworkCIDRs)
	}
	if cfg.GatewayPort != "cr-lrp-test" {
		t.Errorf("GatewayPort = %q, want %q", cfg.GatewayPort, "cr-lrp-test")
	}
	// Unchanged.
	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want %q (should not be overridden)", cfg.BridgeDev, "br-ex")
	}
}

func TestApplyEnvConfigInvalidDuration(t *testing.T) {
	cases := []struct {
		env string
	}{
		{"OVN_NETWORK_RECONCILE_INTERVAL"},
		{"OVN_NETWORK_DRAIN_TIMEOUT"},
		{"OVN_NETWORK_DRAIN_SETTLE_DELAY"},
		{"OVN_NETWORK_STALE_CHASSIS_GRACE_PERIOD"},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			cfg := Config{}
			t.Setenv(tc.env, "notaduration")

			err := applyEnvConfig(&cfg)
			if err == nil {
				t.Fatalf("expected error for %s=notaduration", tc.env)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

func TestApplyEnvConfigInvalidInt(t *testing.T) {
	cases := []struct {
		env string
	}{
		{"OVN_NETWORK_ROUTE_TABLE_ID"},
		{"OVN_NETWORK_VETH_LEAK_TABLE_ID"},
		{"OVN_NETWORK_VETH_LEAK_RULE_PRIORITY"},
		{"OVN_NETWORK_PORT_FORWARD_TABLE_ID"},
		{"OVN_NETWORK_PORT_FORWARD_CT_ZONE"},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			cfg := Config{}
			t.Setenv(tc.env, "notanumber")

			err := applyEnvConfig(&cfg)
			if err == nil {
				t.Fatalf("expected error for %s=notanumber", tc.env)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

func TestApplyFileConfigInvalidDurations(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		key  string
	}{
		{"reconcile_interval", "reconcile_interval: notaduration\n", "reconcile_interval"},
		{"drain_timeout", "drain_timeout: notaduration\n", "drain_timeout"},
		{"drain_settle_delay", "drain_settle_delay: notaduration\n", "drain_settle_delay"},
		{"stale_chassis_grace_period", "stale_chassis_grace_period: notaduration\n", "stale_chassis_grace_period"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			err := applyYAMLConfig(t, &cfg, tc.yaml)
			if err == nil {
				t.Fatalf("expected error for invalid %s", tc.key)
			}
			// The error must name the key as it appears in the file, not the
			// flag or the env var.
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q does not name %s", err, tc.key)
			}
		})
	}
}

func TestLoadConfigInvalidDurationFlags(t *testing.T) {
	cases := []struct {
		flag string
	}{
		{"--reconcile-interval"},
		{"--drain-timeout"},
		{"--drain-settle-delay"},
		{"--stale-chassis-grace-period"},
	}
	for _, tc := range cases {
		t.Run(tc.flag, func(t *testing.T) {
			_, err := loadConfig(fullModeArgs(tc.flag, "notaduration"))
			if err == nil {
				t.Fatalf("expected error for %s notaduration", tc.flag)
			}
			name := strings.TrimLeft(tc.flag, "-")
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
		})
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs())
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-ex")
	}
	if cfg.VRFName != "vrf-provider" {
		t.Errorf("VRFName = %q, want %q", cfg.VRFName, "vrf-provider")
	}
	if cfg.VethNexthop != "169.254.0.1" {
		t.Errorf("VethNexthop = %q, want %q", cfg.VethNexthop, "169.254.0.1")
	}
	if cfg.BridgeIP != "169.254.169.254" {
		t.Errorf("BridgeIP = %q, want %q", cfg.BridgeIP, "169.254.169.254")
	}
	if cfg.ReconcileInterval != 60*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v", cfg.ReconcileInterval, 60*time.Second)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q", cfg.LogLevel, "info")
	}
	if !cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be true by default")
	}
	if cfg.FRRPrefixList != "ANNOUNCED-NETWORKS" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "ANNOUNCED-NETWORKS")
	}
	// VethLeakEnabled was overridden to false for this test; check other defaults.
	if cfg.VethLeakTableID != 200 {
		t.Errorf("VethLeakTableID = %d, want 200", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 2000 {
		t.Errorf("VethLeakRulePriority = %d, want 2000", cfg.VethLeakRulePriority)
	}
}

func TestLoadConfigVethLeakEnabledByDefault(t *testing.T) {
	// VethLeakEnabled defaults to true and requires network-cidr.
	cfg, err := loadConfig(fullModeArgs("--network-cidr", "10.0.0.0/24"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be true by default")
	}
	if cfg.VethProviderIP != "169.254.0.2" {
		t.Errorf("VethProviderIP = %q, want %q (auto-computed from default nexthop)", cfg.VethProviderIP, "169.254.0.2")
	}
}

func TestLoadConfigCLIFlags(t *testing.T) {
	args := []string{
		"--ovn-sb-remote", "tcp:10.0.0.1:6642",
		"--ovn-nb-remote", "tcp:10.0.0.1:6641",
		"--bridge-dev", "br-provider",
		"--network-cidr", "10.0.0.0/24",
		"--gateway-port", "cr-lrp-test",
	}
	cfg, err := loadConfig(args)
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
	if cfg.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", cfg.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
	if len(cfg.NetworkCIDRs) != 1 || cfg.NetworkCIDRs[0] != "10.0.0.0/24" {
		t.Errorf("NetworkCIDRs = %v, want [10.0.0.0/24]", cfg.NetworkCIDRs)
	}
	if len(cfg.NetworkFilters) != 1 {
		t.Fatal("NetworkFilters should have 1 entry when CIDR is set")
	}
	if cfg.GatewayPort != "cr-lrp-test" {
		t.Errorf("GatewayPort = %q, want %q", cfg.GatewayPort, "cr-lrp-test")
	}
}

func TestLoadConfigDryRunFlag(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--dry-run"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true when --dry-run is set")
	}
}

func TestLoadConfigDryRunDefault(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs())
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.DryRun {
		t.Error("DryRun should be false by default")
	}
}

func TestLoadConfigCheckConfigFlag(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		cfg, err := loadConfig(fullModeArgs("--check-config"))
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if !cfg.CheckConfig {
			t.Error("CheckConfig should be true when --check-config is set")
		}
	})

	t.Run("default", func(t *testing.T) {
		cfg, err := loadConfig(fullModeArgs())
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if cfg.CheckConfig {
			t.Error("CheckConfig should be false by default")
		}
	})
}

func TestApplyEnvConfigDryRun(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_DRY_RUN", "true")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true when OVN_NETWORK_DRY_RUN=true")
	}
}

func TestApplyFileConfigDryRun(t *testing.T) {
	cfg := Config{}
	if err := applyYAMLConfig(t, &cfg, "dry_run: true\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if !cfg.DryRun {
		t.Error("DryRun should be true when set in config file")
	}
}

func TestLoadConfigCleanupOnShutdownDefault(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs())
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be true by default")
	}
}

func TestLoadConfigCleanupOnShutdownDisabledViaCLI(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--cleanup-on-shutdown=false"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when --cleanup-on-shutdown=false is set")
	}
}

func TestApplyEnvConfigCleanupOnShutdownFalse(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	t.Setenv("OVN_NETWORK_CLEANUP_ON_SHUTDOWN", "false")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when OVN_NETWORK_CLEANUP_ON_SHUTDOWN=false")
	}
}

func TestApplyEnvConfigDrainOnShutdownFalse(t *testing.T) {
	cfg := Config{DrainOnShutdown: true}
	t.Setenv("OVN_NETWORK_DRAIN_ON_SHUTDOWN", "false")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if cfg.DrainOnShutdown {
		t.Error("DrainOnShutdown should be false when OVN_NETWORK_DRAIN_ON_SHUTDOWN=false")
	}
}

func TestApplyEnvConfigDrainOnShutdownTrue(t *testing.T) {
	cfg := Config{DrainOnShutdown: false}
	t.Setenv("OVN_NETWORK_DRAIN_ON_SHUTDOWN", "true")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if !cfg.DrainOnShutdown {
		t.Error("DrainOnShutdown should be true when OVN_NETWORK_DRAIN_ON_SHUTDOWN=true")
	}
}

func TestApplyEnvConfigDrainOnShutdownOne(t *testing.T) {
	cfg := Config{DrainOnShutdown: false}
	t.Setenv("OVN_NETWORK_DRAIN_ON_SHUTDOWN", "1")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if !cfg.DrainOnShutdown {
		t.Error("DrainOnShutdown should be true when OVN_NETWORK_DRAIN_ON_SHUTDOWN=1")
	}
}

func TestApplyEnvConfigCleanupOnShutdownZero(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	t.Setenv("OVN_NETWORK_CLEANUP_ON_SHUTDOWN", "0")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when OVN_NETWORK_CLEANUP_ON_SHUTDOWN=0")
	}
}

// TestApplyEnvConfigBooleansBothDirections locks in the unified boolean
// env semantics: every boolean env var is honoured in both directions,
// including the directions that were previously dead (e.g.
// CLEANUP_ON_SHUTDOWN was false-only, DRY_RUN was true-only). Each case
// starts from the opposite of the value being set so the change is
// observable.
func TestApplyEnvConfigBooleansBothDirections(t *testing.T) {
	cases := []struct {
		env   string
		value string
		start Config
		want  bool
		get   func(Config) bool
	}{
		// DRY_RUN was true-only: prove the disable direction now applies.
		{"OVN_NETWORK_DRY_RUN", "false", Config{DryRun: true}, false, func(c Config) bool { return c.DryRun }},
		{"OVN_NETWORK_DRY_RUN", "0", Config{DryRun: true}, false, func(c Config) bool { return c.DryRun }},
		{"OVN_NETWORK_DRY_RUN", "true", Config{DryRun: false}, true, func(c Config) bool { return c.DryRun }},
		// CLEANUP_ON_SHUTDOWN was false-only: prove the re-enable direction
		// now overrides a file-level false, per the documented priority.
		{"OVN_NETWORK_CLEANUP_ON_SHUTDOWN", "true", Config{CleanupOnShutdown: false}, true, func(c Config) bool { return c.CleanupOnShutdown }},
		{"OVN_NETWORK_CLEANUP_ON_SHUTDOWN", "false", Config{CleanupOnShutdown: true}, false, func(c Config) bool { return c.CleanupOnShutdown }},
		// DRAIN_ON_SHUTDOWN was already bidirectional; keep it covered.
		{"OVN_NETWORK_DRAIN_ON_SHUTDOWN", "false", Config{DrainOnShutdown: true}, false, func(c Config) bool { return c.DrainOnShutdown }},
		{"OVN_NETWORK_DRAIN_ON_SHUTDOWN", "true", Config{DrainOnShutdown: false}, true, func(c Config) bool { return c.DrainOnShutdown }},
		// VETH_LEAK_ENABLED was false-only: prove the re-enable direction.
		{"OVN_NETWORK_VETH_LEAK_ENABLED", "true", Config{VethLeakEnabled: false}, true, func(c Config) bool { return c.VethLeakEnabled }},
		{"OVN_NETWORK_VETH_LEAK_ENABLED", "false", Config{VethLeakEnabled: true}, false, func(c Config) bool { return c.VethLeakEnabled }},
		// PORT_FORWARD_L3MDEV_ACCEPT was true-only: prove the disable direction.
		{"OVN_NETWORK_PORT_FORWARD_L3MDEV_ACCEPT", "false", Config{PortForwardL3mdevAccept: true}, false, func(c Config) bool { return c.PortForwardL3mdevAccept }},
		{"OVN_NETWORK_PORT_FORWARD_L3MDEV_ACCEPT", "true", Config{PortForwardL3mdevAccept: false}, true, func(c Config) bool { return c.PortForwardL3mdevAccept }},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			cfg := tc.start
			t.Setenv(tc.env, tc.value)
			if err := applyEnvConfig(&cfg); err != nil {
				t.Fatalf("applyEnvConfig: %v", err)
			}
			if got := tc.get(cfg); got != tc.want {
				t.Errorf("%s=%s: field = %v, want %v", tc.env, tc.value, got, tc.want)
			}
		})
	}
}

func TestApplyEnvConfigInvalidBool(t *testing.T) {
	cases := []struct {
		env string
	}{
		{"OVN_NETWORK_DRY_RUN"},
		{"OVN_NETWORK_CLEANUP_ON_SHUTDOWN"},
		{"OVN_NETWORK_DRAIN_ON_SHUTDOWN"},
		{"OVN_NETWORK_VETH_LEAK_ENABLED"},
		{"OVN_NETWORK_PORT_FORWARD_L3MDEV_ACCEPT"},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			cfg := Config{}
			t.Setenv(tc.env, "yes")

			err := applyEnvConfig(&cfg)
			if err == nil {
				t.Fatalf("expected error for %s=yes", tc.env)
			}
			if !strings.Contains(err.Error(), tc.env) {
				t.Errorf("error %q does not name %s", err, tc.env)
			}
		})
	}
}

func TestApplyFileConfigCleanupOnShutdown(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	if err := applyYAMLConfig(t, &cfg, "cleanup_on_shutdown: false\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when set to false in config file")
	}
}

func TestApplyFileConfigCleanupOnShutdownNil(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	// Key absent entirely: an explicit `false` must be distinguishable from
	// "not set", so the prior value has to survive.
	if err := applyYAMLConfig(t, &cfg, "bridge_dev: br-ex\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if !cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should remain true when not set in config file")
	}
}

func TestLoadConfigWithFile(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
bridge_dev: "br-provider"
veth_leak_enabled: false
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
}

func TestLoadConfigCLIOverridesFile(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
bridge_dev: "br-provider"
veth_leak_enabled: false
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path, "--bridge-dev", "br-custom"})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.BridgeDev != "br-custom" {
		t.Errorf("BridgeDev = %q, want %q (CLI should override file)", cfg.BridgeDev, "br-custom")
	}
	// File values should still apply for non-overridden fields.
	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
}

func TestLoadConfigVersionFlag(t *testing.T) {
	_, err := loadConfig([]string{"--version"})
	if !errors.Is(err, errVersionRequested) {
		t.Errorf("expected errVersionRequested, got: %v", err)
	}
}

func TestLoadConfigInvalidNetworkCIDR(t *testing.T) {
	_, err := loadConfig([]string{"--network-cidr", "not-a-cidr"})
	if err == nil {
		t.Error("expected error for invalid CIDR")
	}
}

func TestLoadConfigInvalidVethNexthop(t *testing.T) {
	_, err := loadConfig([]string{"--veth-nexthop", "not-an-ip"})
	if err == nil {
		t.Error("expected error for invalid veth-nexthop")
	}
}

func TestLoadConfigInvalidVRFName(t *testing.T) {
	_, err := loadConfig([]string{"--vrf-name", "bad name; drop"})
	if err == nil {
		t.Error("expected error for invalid VRF name")
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			"valid defaults",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second},
			false,
		},
		{
			"valid with single CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, NetworkCIDRs: []string{"10.0.0.0/24"}},
			false,
		},
		{
			"valid with multiple CIDRs",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, NetworkCIDRs: []string{"10.0.0.0/24", "172.16.0.0/12"}},
			false,
		},
		{
			"invalid nexthop",
			Config{VethNexthop: "bad", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second},
			true,
		},
		{
			"invalid VRF name",
			Config{VethNexthop: "169.254.0.1", VRFName: "bad name", ReconcileInterval: 60 * time.Second},
			true,
		},
		{
			"invalid CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, NetworkCIDRs: []string{"bad"}},
			true,
		},
		{
			"one valid one invalid CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, NetworkCIDRs: []string{"10.0.0.0/24", "bad"}},
			true,
		},
		{
			"valid route table ID",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, RouteTableID: 100},
			false,
		},
		{
			"route table ID zero (main table)",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, RouteTableID: 0},
			false,
		},
		{
			"route table ID max",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, RouteTableID: 252},
			false,
		},
		{
			"route table ID too high",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, RouteTableID: 253},
			true,
		},
		{
			"route table ID negative",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 60 * time.Second, RouteTableID: -1},
			true,
		},
		{
			"reconcile interval zero",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: 0},
			true,
		},
		{
			"reconcile interval negative",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", ReconcileInterval: -1 * time.Second},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(&tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigRejectsNonPositiveReconcileInterval(t *testing.T) {
	t.Run("flag zero", func(t *testing.T) {
		_, err := loadConfig(fullModeArgs("--reconcile-interval", "0s"))
		if err == nil {
			t.Fatal("expected error for --reconcile-interval 0s")
		}
	})

	t.Run("flag negative", func(t *testing.T) {
		_, err := loadConfig(fullModeArgs("--reconcile-interval", "-5s"))
		if err == nil {
			t.Fatal("expected error for --reconcile-interval -5s")
		}
	})

	t.Run("file zero", func(t *testing.T) {
		content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
veth_leak_enabled: false
reconcile_interval: "0s"
`
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write test config: %v", err)
		}
		if _, err := loadConfig([]string{"--config", path}); err == nil {
			t.Fatal("expected error for reconcile_interval 0s in config file")
		}
	})
}

func TestLoadConfigRouteTableIDCLI(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--route-table-id", "100"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.RouteTableID != 100 {
		t.Errorf("RouteTableID = %d, want 100", cfg.RouteTableID)
	}
}

func TestLoadConfigRouteTableIDInvalid(t *testing.T) {
	_, err := loadConfig([]string{"--route-table-id", "253"})
	if err == nil {
		t.Error("expected error for route-table-id 253")
	}
}

func TestApplyEnvConfigRouteTableID(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_ROUTE_TABLE_ID", "42")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}

	if cfg.RouteTableID != 42 {
		t.Errorf("RouteTableID = %d, want 42", cfg.RouteTableID)
	}
}

func TestEffectiveNetworkFilters(t *testing.T) {
	_, manual, _ := net.ParseCIDR("10.0.0.0/24")
	_, discovered, _ := net.ParseCIDR("198.51.100.0/24")

	t.Run("manual takes precedence", func(t *testing.T) {
		got := effectiveNetworkFilters([]*net.IPNet{manual}, []*net.IPNet{discovered})
		if len(got) != 1 || got[0].String() != "10.0.0.0/24" {
			t.Errorf("expected manual, got %v", got)
		}
	})

	t.Run("discovered when no manual", func(t *testing.T) {
		got := effectiveNetworkFilters(nil, []*net.IPNet{discovered})
		if len(got) != 1 || got[0].String() != "198.51.100.0/24" {
			t.Errorf("expected discovered, got %v", got)
		}
	})

	t.Run("nil when both empty", func(t *testing.T) {
		got := effectiveNetworkFilters(nil, nil)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})
}

func TestIsValidIdentifier(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"vrf-provider", true},
		{"vrf_provider", true},
		{"vrf.provider", true},
		{"VRF123", true},
		{"", false},
		{"bad name", false},
		{"bad;name", false},
		{"bad$name", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isValidIdentifier(tt.input)
			if got != tt.want {
				t.Errorf("isValidIdentifier(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadConfigVethLeakWithoutNetworkCIDR(t *testing.T) {
	// Veth leak no longer requires network-cidr — networks are auto-discovered from OVN.
	cfg, err := loadConfig(fullModeArgs("--veth-leak-enabled"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be true")
	}
}

func TestLoadConfigVethLeakDisabledWithoutNetworkCIDR(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--veth-leak-enabled=false"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be false")
	}
}

func TestLoadConfigVethLeakAutoProviderIP(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs(
		"--veth-nexthop", "169.254.0.1",
		"--network-cidr", "10.0.0.0/24",
	))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.VethProviderIP != "169.254.0.2" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.2")
	}
}

func TestLoadConfigVethLeakExplicitProviderIP(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs(
		"--veth-provider-ip", "169.254.0.10",
		"--network-cidr", "10.0.0.0/24",
	))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.VethProviderIP != "169.254.0.10" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.10")
	}
}

func TestLoadConfigVethLeakTableIDConflict(t *testing.T) {
	_, err := loadConfig([]string{
		"--route-table-id", "200",
		"--veth-leak-table-id", "200",
		"--network-cidr", "10.0.0.0/24",
	})
	if err == nil {
		t.Error("expected error when veth-leak-table-id equals route-table-id")
	}
}

func TestLoadConfigVethLeakTableIDInvalid(t *testing.T) {
	_, err := loadConfig([]string{
		"--veth-leak-table-id", "0",
		"--network-cidr", "10.0.0.0/24",
	})
	if err == nil {
		t.Error("expected error for veth-leak-table-id 0")
	}
}

func TestApplyEnvConfigVethLeak(t *testing.T) {
	cfg := Config{VethLeakEnabled: true}
	t.Setenv("OVN_NETWORK_VETH_LEAK_ENABLED", "false")
	t.Setenv("OVN_NETWORK_VETH_PROVIDER_IP", "169.254.0.5")
	t.Setenv("OVN_NETWORK_VETH_LEAK_TABLE_ID", "201")
	t.Setenv("OVN_NETWORK_VETH_LEAK_RULE_PRIORITY", "3000")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}

	if cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be false")
	}
	if cfg.VethProviderIP != "169.254.0.5" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.5")
	}
	if cfg.VethLeakTableID != 201 {
		t.Errorf("VethLeakTableID = %d, want 201", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 3000 {
		t.Errorf("VethLeakRulePriority = %d, want 3000", cfg.VethLeakRulePriority)
	}
}

func TestApplyFileConfigVethLeak(t *testing.T) {
	cfg := Config{VethLeakEnabled: true, VethLeakTableID: 200, VethLeakRulePriority: 2000}
	if err := applyYAMLConfig(t, &cfg, `
veth_leak_enabled: false
veth_provider_ip: "169.254.0.5"
veth_leak_table_id: 201
veth_leak_rule_priority: 3000
`); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}

	if cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be false")
	}
	if cfg.VethProviderIP != "169.254.0.5" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.5")
	}
	if cfg.VethLeakTableID != 201 {
		t.Errorf("VethLeakTableID = %d, want 201", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 3000 {
		t.Errorf("VethLeakRulePriority = %d, want 3000", cfg.VethLeakRulePriority)
	}
}

func TestLoadConfigVethLeakYAML(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
network_cidr: "10.0.0.0/24"
veth_leak_enabled: true
veth_provider_ip: "169.254.0.5"
veth_leak_table_id: 201
veth_leak_rule_priority: 3000
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.VethLeakEnabled {
		t.Error("VethLeakEnabled should be true")
	}
	if cfg.VethProviderIP != "169.254.0.5" {
		t.Errorf("VethProviderIP = %q, want %q", cfg.VethProviderIP, "169.254.0.5")
	}
	if cfg.VethLeakTableID != 201 {
		t.Errorf("VethLeakTableID = %d, want 201", cfg.VethLeakTableID)
	}
	if cfg.VethLeakRulePriority != 3000 {
		t.Errorf("VethLeakRulePriority = %d, want 3000", cfg.VethLeakRulePriority)
	}
}

func TestLoadConfigFRRPrefixListCLI(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--frr-prefix-list", "ANNOUNCED-NETWORKS"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.FRRPrefixList != "ANNOUNCED-NETWORKS" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "ANNOUNCED-NETWORKS")
	}
}

func TestLoadConfigFRRPrefixListInvalid(t *testing.T) {
	_, err := loadConfig([]string{"--frr-prefix-list", "bad name; drop"})
	if err == nil {
		t.Error("expected error for invalid frr-prefix-list name")
	}
}

func TestLoadConfigStaleChassisGracePeriodDefault(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs())
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 5*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 5*time.Minute)
	}
}

func TestLoadConfigStaleChassisGracePeriodCLI(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--stale-chassis-grace-period", "10m"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 10*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 10*time.Minute)
	}
}

func TestLoadConfigStaleChassisGracePeriodDisabled(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--stale-chassis-grace-period", "0s"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 0 {
		t.Errorf("StaleChassisGracePeriod = %v, want 0 (disabled)", cfg.StaleChassisGracePeriod)
	}
}

func TestApplyEnvConfigStaleChassisGracePeriod(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	t.Setenv("OVN_NETWORK_STALE_CHASSIS_GRACE_PERIOD", "3m")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 3*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 3*time.Minute)
	}
}

func TestApplyFileConfigStaleChassisGracePeriod(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	if err := applyYAMLConfig(t, &cfg, "stale_chassis_grace_period: 2m\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 2*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 2*time.Minute)
	}
}

func TestApplyFileConfigStaleChassisGracePeriodEmpty(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	if err := applyYAMLConfig(t, &cfg, "bridge_dev: br-ex\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 5*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v (should keep default)", cfg.StaleChassisGracePeriod, 5*time.Minute)
	}
}

func TestLoadConfigStaleChassisGracePeriodYAML(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
veth_leak_enabled: false
stale_chassis_grace_period: "7m"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.StaleChassisGracePeriod != 7*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 7*time.Minute)
	}
}

func TestValidateConfigStaleChassisGracePeriodNegative(t *testing.T) {
	cfg := Config{
		VethNexthop:             "169.254.0.1",
		VRFName:                 "vrf-provider",
		ReconcileInterval:       60 * time.Second,
		StaleChassisGracePeriod: -1 * time.Minute,
	}
	err := validateConfig(&cfg)
	if err == nil {
		t.Error("expected error for negative stale-chassis-grace-period")
	}
}

func TestLoadConfigDrainSettleDelayDefault(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs())
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.DrainSettleDelay != 500*time.Millisecond {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 500*time.Millisecond)
	}
}

func TestLoadConfigDrainSettleDelayCLI(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--drain-settle-delay", "8s"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.DrainSettleDelay != 8*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 8*time.Second)
	}
}

func TestLoadConfigDrainSettleDelayDisabled(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--drain-settle-delay", "0s"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.DrainSettleDelay != 0 {
		t.Errorf("DrainSettleDelay = %v, want 0 (disabled)", cfg.DrainSettleDelay)
	}
}

func TestApplyEnvConfigDrainSettleDelay(t *testing.T) {
	cfg := Config{DrainSettleDelay: 3 * time.Second}
	t.Setenv("OVN_NETWORK_DRAIN_SETTLE_DELAY", "5s")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if cfg.DrainSettleDelay != 5*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 5*time.Second)
	}
}

func TestApplyFileConfigDrainSettleDelay(t *testing.T) {
	cfg := Config{DrainSettleDelay: 3 * time.Second}
	if err := applyYAMLConfig(t, &cfg, "drain_settle_delay: 10s\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.DrainSettleDelay != 10*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 10*time.Second)
	}
}

func TestApplyFileConfigDrainSettleDelayEmpty(t *testing.T) {
	cfg := Config{DrainSettleDelay: 3 * time.Second}
	if err := applyYAMLConfig(t, &cfg, "bridge_dev: br-ex\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.DrainSettleDelay != 3*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v (should keep default)", cfg.DrainSettleDelay, 3*time.Second)
	}
}

func TestLoadConfigDrainSettleDelayYAML(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
veth_leak_enabled: false
drain_settle_delay: "4s"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.DrainSettleDelay != 4*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 4*time.Second)
	}
}

func TestValidateConfigDrainSettleDelayNegative(t *testing.T) {
	cfg := Config{
		VethNexthop:       "169.254.0.1",
		VRFName:           "vrf-provider",
		ReconcileInterval: 60 * time.Second,
		DrainSettleDelay:  -1 * time.Second,
	}
	err := validateConfig(&cfg)
	if err == nil {
		t.Error("expected error for negative drain-settle-delay")
	}
}

func TestApplyEnvConfigFRRPrefixList(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_FRR_PREFIX_LIST", "MY-LIST")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if cfg.FRRPrefixList != "MY-LIST" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "MY-LIST")
	}
}

func TestApplyFileConfigFRRPrefixList(t *testing.T) {
	cfg := Config{}
	if err := applyYAMLConfig(t, &cfg, "frr_prefix_list: FILE-LIST\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.FRRPrefixList != "FILE-LIST" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "FILE-LIST")
	}
}

func TestPortForwardConfigParsing(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
port_forward_dev: "loopback1"
port_forward_table_id: 202
port_forwards:
  - vip: "198.51.100.10"
    manage_vip: true
    rules:
      - proto: tcp
        port: 80
        dest_addr: "10.0.0.100"
      - proto: udp
        port: 53
        dest_addr: "10.0.0.200"
        dest_port: 1053
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("PortForwardDev = %q, want %q", cfg.PortForwardDev, "loopback1")
	}
	if cfg.PortForwardTableID != 202 {
		t.Errorf("PortForwardTableID = %d, want %d", cfg.PortForwardTableID, 202)
	}
	if !cfg.PortForwardEnabled {
		t.Error("PortForwardEnabled should be true")
	}
	if len(cfg.PortForwards) != 1 {
		t.Fatalf("len(PortForwards) = %d, want 1", len(cfg.PortForwards))
	}
	pf := cfg.PortForwards[0]
	if pf.VIP != "198.51.100.10" {
		t.Errorf("VIP = %q, want %q", pf.VIP, "198.51.100.10")
	}
	if !pf.ManageVIP {
		t.Error("ManageVIP should be true")
	}
	if len(pf.Rules) != 2 {
		t.Fatalf("len(Rules) = %d, want 2", len(pf.Rules))
	}
	if pf.Rules[1].DestPort != 1053 {
		t.Errorf("Rules[1].DestPort = %d, want 1053", pf.Rules[1].DestPort)
	}
}

func TestPortForwardValidation(t *testing.T) {
	base := func() Config {
		return Config{
			VethNexthop:        "169.254.0.1",
			ReconcileInterval:  60 * time.Second,
			VethLeakEnabled:    true,
			VethLeakTableID:    200,
			PortForwardDev:     "loopback1",
			PortForwardTableID: 201,
			PortForwardCTZone:  64000,
			PortForwards: []PortForwardVIP{
				{
					VIP: "198.51.100.10",
					Rules: []PortForwardRule{
						{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
					},
				},
			},
		}
	}

	t.Run("valid", func(t *testing.T) {
		cfg := base()
		if err := validateConfig(&cfg); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if !cfg.PortForwardEnabled {
			t.Error("PortForwardEnabled should be true")
		}
	})

	t.Run("disabled_when_no_forwards", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards = nil
		if err := validateConfig(&cfg); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if cfg.PortForwardEnabled {
			t.Error("PortForwardEnabled should be false when no forwards")
		}
	})

	t.Run("invalid_table_id", func(t *testing.T) {
		cfg := base()
		cfg.PortForwardTableID = 300
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for invalid table ID")
		}
	})

	t.Run("table_id_conflict_route", func(t *testing.T) {
		cfg := base()
		cfg.RouteTableID = 201
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for table ID conflict with route_table_id")
		}
	})

	t.Run("table_id_conflict_leak", func(t *testing.T) {
		cfg := base()
		cfg.PortForwardTableID = 200 // same as VethLeakTableID
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for table ID conflict with veth_leak_table_id")
		}
	})

	t.Run("requires_veth_leak", func(t *testing.T) {
		cfg := base()
		cfg.VethLeakEnabled = false
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error when veth leak disabled")
		}
	})

	t.Run("invalid_vip", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].VIP = "not-an-ip"
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for invalid VIP")
		}
	})

	t.Run("ipv6_vip_rejected", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].VIP = "2001:db8::1"
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for IPv6 VIP")
		}
	})

	t.Run("no_rules", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules = nil
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for empty rules")
		}
	})

	t.Run("invalid_proto", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].Proto = "icmp"
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for invalid proto")
		}
	})

	t.Run("invalid_port", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].Port = 0
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for port 0")
		}
	})

	t.Run("invalid_dest_addr", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].DestAddr = "invalid"
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for invalid dest_addr")
		}
	})

	t.Run("ipv6_dest_addr_rejected", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].DestAddr = "::1"
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for IPv6 dest_addr")
		}
	})

	t.Run("duplicate_vip", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards = append(cfg.PortForwards, PortForwardVIP{
			VIP:   "198.51.100.10", // same as first entry
			Rules: []PortForwardRule{{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"}},
		})
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for duplicate VIP")
		}
	})

	t.Run("duplicate_rule", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules = append(cfg.PortForwards[0].Rules,
			PortForwardRule{Proto: "tcp", Port: 80, DestAddr: "10.0.0.200"},
		)
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for duplicate proto/port on same VIP")
		}
	})

	t.Run("same_port_different_proto_ok", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules = append(cfg.PortForwards[0].Rules,
			PortForwardRule{Proto: "udp", Port: 80, DestAddr: "10.0.0.200"},
		)
		if err := validateConfig(&cfg); err != nil {
			t.Errorf("same port with different proto should be valid: %v", err)
		}
	})

	// 0 and 254 are both kernel aliases for the "main" routing table, but the
	// validator does not collapse them. Whatever the resolution, it must not
	// silently let the pair through and end up with two different agent
	// subsystems writing into the same on-disk table.
	t.Run("route_table_id_0_collides_with_port_forward_main_alias", func(t *testing.T) {
		cfg := base()
		cfg.RouteTableID = 0         // main table (alias)
		cfg.PortForwardTableID = 254 // main table (canonical id)
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error: port_forward_table_id 254 (main) must not coexist with route_table_id 0 (main alias)")
		}
	})

	t.Run("route_table_id_0_collides_with_veth_leak_main_alias", func(t *testing.T) {
		cfg := base()
		cfg.RouteTableID = 0      // main table (alias)
		cfg.VethLeakTableID = 254 // main table (canonical id)
		// Use the matching port-forward range so the test fails specifically
		// on the route/leak collision, not on an unrelated range check.
		cfg.PortForwardTableID = 201
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error: veth_leak_table_id 254 (main) must not coexist with route_table_id 0 (main alias)")
		}
	})
}

func TestApplyEnvConfigPortForward(t *testing.T) {
	cfg := Config{
		PortForwardDev:     "loopback1",
		PortForwardTableID: 201,
	}

	t.Setenv("OVN_NETWORK_PORT_FORWARD_DEV", "loopback1")
	t.Setenv("OVN_NETWORK_PORT_FORWARD_TABLE_ID", "202")

	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("PortForwardDev = %q, want %q", cfg.PortForwardDev, "loopback1")
	}
	if cfg.PortForwardTableID != 202 {
		t.Errorf("PortForwardTableID = %d, want %d", cfg.PortForwardTableID, 202)
	}
}

func TestLoadConfigPortForwardCLIFlags(t *testing.T) {
	cfg, err := loadConfig([]string{
		"--ovn-sb-remote", "tcp:10.0.0.1:6642",
		"--ovn-nb-remote", "tcp:10.0.0.1:6641",
		"--port-forward-dev", "loopback1",
		"--port-forward-table-id", "203",
	})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("PortForwardDev = %q, want %q", cfg.PortForwardDev, "loopback1")
	}
	if cfg.PortForwardTableID != 203 {
		t.Errorf("PortForwardTableID = %d, want %d", cfg.PortForwardTableID, 203)
	}
}

func TestApplyFileConfigPortForward(t *testing.T) {
	cfg := Config{
		PortForwardDev:     "loopback1",
		PortForwardTableID: 201,
	}
	if err := applyYAMLConfig(t, &cfg, `
port_forward_dev: "loopback1"
port_forward_table_id: 205
port_forwards:
  - vip: "198.51.100.10"
    rules:
      - proto: tcp
        port: 80
        dest_addr: "10.0.0.100"
`); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("PortForwardDev = %q, want %q", cfg.PortForwardDev, "loopback1")
	}
	if cfg.PortForwardTableID != 205 {
		t.Errorf("PortForwardTableID = %d, want %d", cfg.PortForwardTableID, 205)
	}
	if len(cfg.PortForwards) != 1 {
		t.Fatalf("len(PortForwards) = %d, want 1", len(cfg.PortForwards))
	}
	pf := cfg.PortForwards[0]
	if pf.VIP != "198.51.100.10" || len(pf.Rules) != 1 || pf.Rules[0].DestAddr != "10.0.0.100" {
		t.Errorf("PortForwards[0] = %+v, want the nested rule decoded", pf)
	}
}

func TestApplyFileConfigPortForwardEmpty(t *testing.T) {
	cfg := Config{
		PortForwardDev:     "loopback1",
		PortForwardTableID: 201,
	}
	if err := applyYAMLConfig(t, &cfg, "bridge_dev: br-ex\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("PortForwardDev should remain %q, got %q", "loopback1", cfg.PortForwardDev)
	}
	if cfg.PortForwardTableID != 201 {
		t.Errorf("PortForwardTableID should remain %d, got %d", 201, cfg.PortForwardTableID)
	}
	if len(cfg.PortForwards) != 0 {
		t.Errorf("PortForwards should remain empty, got %+v", cfg.PortForwards)
	}
}

func TestPortForwardDefaults(t *testing.T) {
	cfg, err := loadConfig([]string{
		"--ovn-sb-remote", "tcp:10.0.0.1:6642",
		"--ovn-nb-remote", "tcp:10.0.0.1:6641",
	})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("default PortForwardDev = %q, want %q", cfg.PortForwardDev, "loopback1")
	}
	if cfg.PortForwardTableID != 201 {
		t.Errorf("default PortForwardTableID = %d, want %d", cfg.PortForwardTableID, 201)
	}
	if cfg.PortForwardEnabled {
		t.Error("PortForwardEnabled should be false by default")
	}
}

func TestDestAddrsHelper(t *testing.T) {
	tests := []struct {
		name     string
		rule     PortForwardRule
		wantLen  int
		wantAddr string // first element, if any
	}{
		{
			name:     "dest_addrs_set",
			rule:     PortForwardRule{DestAddrs: []string{"10.0.0.1", "10.0.0.2"}},
			wantLen:  2,
			wantAddr: "10.0.0.1",
		},
		{
			name:     "dest_addr_set",
			rule:     PortForwardRule{DestAddr: "10.0.0.1"},
			wantLen:  1,
			wantAddr: "10.0.0.1",
		},
		{
			name:    "neither_set",
			rule:    PortForwardRule{},
			wantLen: 0,
		},
		{
			name:     "dest_addrs_takes_precedence",
			rule:     PortForwardRule{DestAddr: "10.0.0.99", DestAddrs: []string{"10.0.0.1"}},
			wantLen:  1,
			wantAddr: "10.0.0.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.rule.destAddrs()
			if len(got) != tt.wantLen {
				t.Fatalf("destAddrs() len = %d, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen > 0 && got[0] != tt.wantAddr {
				t.Errorf("destAddrs()[0] = %q, want %q", got[0], tt.wantAddr)
			}
		})
	}
}

func TestPortForwardMultiBackendValidation(t *testing.T) {
	base := func() Config {
		return Config{
			VethNexthop:        "169.254.0.1",
			ReconcileInterval:  60 * time.Second,
			VethLeakEnabled:    true,
			VethLeakTableID:    200,
			PortForwardDev:     "loopback1",
			PortForwardTableID: 201,
			PortForwardCTZone:  64000,
			PortForwards: []PortForwardVIP{
				{
					VIP: "198.51.100.10",
					Rules: []PortForwardRule{
						{Proto: "udp", Port: 53, DestAddrs: []string{"10.0.0.200", "10.0.0.201"}, DestPort: 1053},
					},
				},
			},
		}
	}

	t.Run("valid_multi_backend", func(t *testing.T) {
		cfg := base()
		if err := validateConfig(&cfg); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("dest_addr_and_dest_addrs_mutually_exclusive", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].DestAddr = "10.0.0.200"
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error when both dest_addr and dest_addrs are set")
		}
	})

	t.Run("no_dest_addr_at_all", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].DestAddrs = nil
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error when neither dest_addr nor dest_addrs is set")
		}
	})

	t.Run("invalid_addr_in_dest_addrs", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].DestAddrs = []string{"10.0.0.200", "invalid"}
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for invalid address in dest_addrs")
		}
	})

	t.Run("ipv6_in_dest_addrs", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].DestAddrs = []string{"10.0.0.200", "::1"}
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for IPv6 in dest_addrs")
		}
	})

	t.Run("duplicate_in_dest_addrs", func(t *testing.T) {
		cfg := base()
		cfg.PortForwards[0].Rules[0].DestAddrs = []string{"10.0.0.200", "10.0.0.200"}
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error for duplicate address in dest_addrs")
		}
	})

	t.Run("too_many_backends", func(t *testing.T) {
		cfg := base()
		addrs := make([]string, 257)
		for i := range addrs {
			addrs[i] = fmt.Sprintf("10.%d.%d.%d", i/65536%256, i/256%256, i%256+1)
		}
		cfg.PortForwards[0].Rules[0].DestAddrs = addrs
		if err := validateConfig(&cfg); err == nil {
			t.Error("expected error when dest_addrs exceeds max backends")
		}
	})
}

func TestPortForwardConfigParsingDestAddrs(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
port_forwards:
  - vip: "198.51.100.10"
    manage_vip: true
    rules:
      - proto: udp
        port: 53
        dest_addrs:
          - "10.0.0.200"
          - "10.0.0.201"
          - "10.0.0.202"
        dest_port: 1053
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}

	if len(cfg.PortForwards) != 1 {
		t.Fatalf("len(PortForwards) = %d, want 1", len(cfg.PortForwards))
	}
	r := cfg.PortForwards[0].Rules[0]
	if len(r.DestAddrs) != 3 {
		t.Fatalf("len(DestAddrs) = %d, want 3", len(r.DestAddrs))
	}
	if r.DestAddrs[0] != "10.0.0.200" || r.DestAddrs[1] != "10.0.0.201" || r.DestAddrs[2] != "10.0.0.202" {
		t.Errorf("DestAddrs = %v, want [10.0.0.200 10.0.0.201 10.0.0.202]", r.DestAddrs)
	}
}

// VethProviderIP is auto-computed as nextIPInSubnet(VethNexthop). With
// VethNexthop=255.255.255.255 the helper wraps to 0.0.0.0 (see
// TestNextIPInSubnet) and the subsequent net.ParseIP check accepts it — so
// the validator ends up with a wrap-around address it would normally reject
// from explicit config. Document the current "wrap + accept" behaviour so
// any caller-side validation added later (e.g. rejecting an unspecified
// VethProviderIP) trips this test instead of silently changing the contract.
func TestVethProviderIPAutoComputeWrapsAt255_255_255_255(t *testing.T) {
	cfg := Config{
		VethNexthop:       "255.255.255.255",
		ReconcileInterval: 60 * time.Second,
		VethLeakEnabled:   true,
		VethLeakTableID:   200,
		// VethProviderIP intentionally unset — triggers auto-compute.
	}

	if err := validateConfig(&cfg); err != nil {
		t.Fatalf("validateConfig() error: %v", err)
	}
	if cfg.VethProviderIP != "0.0.0.0" {
		t.Errorf("VethProviderIP auto-compute at 255.255.255.255 = %q, want 0.0.0.0 (wrap)", cfg.VethProviderIP)
	}
}

func TestNextIPInSubnet(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"169.254.0.1", "169.254.0.2"},
		{"169.254.0.254", "169.254.0.255"},
		{"169.254.0.255", "169.254.1.0"},
		{"10.0.0.0", "10.0.0.1"},
		{"255.255.255.255", "0.0.0.0"}, // wraps around
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := nextIPInSubnet(net.ParseIP(tt.input))
			if got.String() != tt.want {
				t.Errorf("nextIPInSubnet(%s) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

func TestLoadConfigMetricsListenDefaultDisabled(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs())
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.MetricsListen != "" {
		t.Errorf("MetricsListen default = %q, want empty (disabled)", cfg.MetricsListen)
	}
}

func TestLoadConfigMetricsListenViaCLI(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs("--metrics-listen", "127.0.0.1:9273"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.MetricsListen != "127.0.0.1:9273" {
		t.Errorf("MetricsListen = %q, want %q", cfg.MetricsListen, "127.0.0.1:9273")
	}
}

func TestApplyEnvConfigMetricsListen(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_METRICS_LISTEN", "0.0.0.0:9273")
	if err := applyEnvConfig(&cfg); err != nil {
		t.Fatalf("applyEnvConfig: %v", err)
	}
	if cfg.MetricsListen != "0.0.0.0:9273" {
		t.Errorf("MetricsListen = %q, want %q", cfg.MetricsListen, "0.0.0.0:9273")
	}
}

func TestApplyFileConfigMetricsListen(t *testing.T) {
	cfg := Config{}
	if err := applyYAMLConfig(t, &cfg, "metrics_listen: \"127.0.0.1:9273\"\n"); err != nil {
		t.Fatalf("applyFileConfig: %v", err)
	}
	if cfg.MetricsListen != "127.0.0.1:9273" {
		t.Errorf("MetricsListen = %q, want %q", cfg.MetricsListen, "127.0.0.1:9273")
	}
}

func TestLoadConfigMetricsListenInvalid(t *testing.T) {
	_, err := loadConfig([]string{
		"--ovn-sb-remote", "tcp:10.0.0.1:6642",
		"--ovn-nb-remote", "tcp:10.0.0.1:6641",
		"--metrics-listen", "no-port-here",
	})
	if err == nil {
		t.Fatal("expected validation error for malformed metrics-listen")
	}
}

func TestLoadConfigCLIOverridesEnvMetricsListen(t *testing.T) {
	t.Setenv("OVN_NETWORK_METRICS_LISTEN", "127.0.0.1:9000")
	cfg, err := loadConfig(fullModeArgs("--metrics-listen", "127.0.0.1:9273"))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.MetricsListen != "127.0.0.1:9273" {
		t.Errorf("MetricsListen = %q, want %q (CLI > env)", cfg.MetricsListen, "127.0.0.1:9273")
	}
}

// portForwardVIPs returns a minimal valid port-forward configuration for
// operating-mode tests.
func portForwardVIPs() []PortForwardVIP {
	return []PortForwardVIP{
		{
			VIP:       "198.51.100.10",
			ManageVIP: true,
			Rules:     []PortForwardRule{{Proto: "tcp", Port: 443, DestAddr: "10.0.0.100"}},
		},
	}
}

// TestValidateMode covers the OVN-remote/port-forward matrix from issue #121:
// the four combinations of OVN remotes and port_forwards and the mode (or
// error) each produces.
func TestValidateMode(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		wantErr    bool
		wantPFOnly bool
	}{
		{
			"both remotes set: full mode",
			Config{OVNSBRemote: "tcp:10.0.0.1:6642", OVNNBRemote: "tcp:10.0.0.1:6641"},
			false, false,
		},
		{
			"both remotes set with port forwards: full mode",
			Config{OVNSBRemote: "tcp:10.0.0.1:6642", OVNNBRemote: "tcp:10.0.0.1:6641", PortForwards: portForwardVIPs()},
			false, false,
		},
		{
			"no remotes, port forwards set: port-forward-only mode",
			Config{PortForwards: portForwardVIPs()},
			false, true,
		},
		{
			"no remotes, no port forwards: error",
			Config{},
			true, false,
		},
		{
			"only SB remote set: error",
			Config{OVNSBRemote: "tcp:10.0.0.1:6642"},
			true, false,
		},
		{
			"only NB remote set: error",
			Config{OVNNBRemote: "tcp:10.0.0.1:6641"},
			true, false,
		},
		{
			"only SB remote set with port forwards: error",
			Config{OVNSBRemote: "tcp:10.0.0.1:6642", PortForwards: portForwardVIPs()},
			true, false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			err := validateMode(&cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && cfg.PortForwardOnly != tt.wantPFOnly {
				t.Errorf("PortForwardOnly = %v, want %v", cfg.PortForwardOnly, tt.wantPFOnly)
			}
		})
	}
}

// TestValidateModeMasquerade verifies that masquerade options depending on
// OVN-derived state are rejected in port-forward-only mode but accepted in
// full mode.
func TestValidateModeMasquerade(t *testing.T) {
	withVIP := func(mut func(*PortForwardVIP)) []PortForwardVIP {
		v := portForwardVIPs()
		mut(&v[0])
		return v
	}

	t.Run("router_masquerade rejected in port-forward-only mode", func(t *testing.T) {
		cfg := Config{PortForwards: withVIP(func(v *PortForwardVIP) { v.RouterMasquerade = true })}
		if err := validateMode(&cfg); err == nil {
			t.Error("expected error: router_masquerade requires OVN")
		}
	})

	t.Run("hairpin_masquerade rejected without network_cidr", func(t *testing.T) {
		cfg := Config{PortForwards: withVIP(func(v *PortForwardVIP) { v.HairpinMasquerade = true })}
		if err := validateMode(&cfg); err == nil {
			t.Error("expected error: hairpin_masquerade needs network_cidr in port-forward-only mode")
		}
	})

	t.Run("hairpin_masquerade allowed with network_cidr", func(t *testing.T) {
		cfg := Config{
			NetworkCIDRs: []string{"203.0.113.0/24"},
			PortForwards: withVIP(func(v *PortForwardVIP) { v.HairpinMasquerade = true }),
		}
		if err := validateMode(&cfg); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("router_masquerade allowed in full mode", func(t *testing.T) {
		cfg := Config{
			OVNSBRemote:  "tcp:10.0.0.1:6642",
			OVNNBRemote:  "tcp:10.0.0.1:6641",
			PortForwards: withVIP(func(v *PortForwardVIP) { v.RouterMasquerade = true }),
		}
		if err := validateMode(&cfg); err != nil {
			t.Errorf("router_masquerade must be allowed in full mode: %v", err)
		}
	})
}

// TestLoadConfigPortForwardOnlyMode verifies that a config file with only
// port_forwards (no OVN remotes) loads successfully and derives
// port-forward-only mode.
func TestLoadConfigPortForwardOnlyMode(t *testing.T) {
	content := `
port_forwards:
  - vip: "198.51.100.10"
    manage_vip: true
    rules:
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := loadConfig([]string{"--config", path})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if !cfg.PortForwardOnly {
		t.Error("PortForwardOnly should be true with port_forwards and no OVN remotes")
	}
	if !cfg.PortForwardEnabled {
		t.Error("PortForwardEnabled should be true")
	}
}

// TestLoadConfigRejectsEmptyMode verifies that a config with neither OVN
// remotes nor port_forwards is rejected with a clear error.
func TestLoadConfigRejectsEmptyMode(t *testing.T) {
	if _, err := loadConfig(nil); err == nil {
		t.Error("expected error when neither OVN remotes nor port_forwards are configured")
	}
}

// TestLoadConfigRejectsIncompleteOVN verifies that setting only one of the
// two OVN remotes is rejected.
func TestLoadConfigRejectsIncompleteOVN(t *testing.T) {
	t.Run("only SB", func(t *testing.T) {
		if _, err := loadConfig([]string{"--ovn-sb-remote", "tcp:10.0.0.1:6642"}); err == nil {
			t.Error("expected error when only ovn-sb-remote is set")
		}
	})
	t.Run("only NB", func(t *testing.T) {
		if _, err := loadConfig([]string{"--ovn-nb-remote", "tcp:10.0.0.1:6641"}); err == nil {
			t.Error("expected error when only ovn-nb-remote is set")
		}
	})
}

// TestConfigOptionsRegistry guards the registry's core promise: an option is
// declared once, and its three names (flag, environment variable, config-file
// key) are derived from that one declaration rather than repeated. If a future
// row hand-writes an inconsistent name, or a duplicate slips in, this fails.
func TestConfigOptionsRegistry(t *testing.T) {
	opts := configOptions()
	if len(opts) == 0 {
		t.Fatal("configOptions() is empty")
	}

	seenFlag := map[string]bool{}
	seenKey := map[string]bool{}
	seenEnv := map[string]bool{}

	for _, o := range opts {
		if o.Key == "" {
			t.Errorf("option %+v has no config-file key", o)
		}
		if o.Usage == "" {
			t.Errorf("option %q has no usage text", o.Key)
		}
		if seenKey[o.Key] {
			t.Errorf("duplicate config-file key %q", o.Key)
		}
		seenKey[o.Key] = true

		// Every option must be settable from a config file.
		if o.applyYAML == nil {
			t.Errorf("option %q has no YAML binding", o.Key)
		}

		if o.Flag == "" {
			// A YAML-only option must have no flag or env plumbing at all.
			if o.register != nil || o.applyFlag != nil || o.applyEnv != nil {
				t.Errorf("YAML-only option %q must not declare flag/env bindings", o.Key)
			}
			if o.EnvVar() != "" {
				t.Errorf("YAML-only option %q must have no env var, got %q", o.Key, o.EnvVar())
			}
			continue
		}

		if seenFlag[o.Flag] {
			t.Errorf("duplicate flag %q", o.Flag)
		}
		seenFlag[o.Flag] = true
		if seenEnv[o.EnvVar()] {
			t.Errorf("duplicate env var %q", o.EnvVar())
		}
		seenEnv[o.EnvVar()] = true

		// A flagged option must carry the full set of bindings.
		if o.register == nil || o.applyFlag == nil || o.applyEnv == nil {
			t.Errorf("option %q is missing a flag/env binding", o.Flag)
		}

		// The derivation itself: names must follow mechanically from the flag.
		wantKey := strings.ReplaceAll(o.Flag, "-", "_")
		if o.Key != wantKey {
			t.Errorf("option %q: Key = %q, want %q (derived from the flag)", o.Flag, o.Key, wantKey)
		}
		wantEnv := "OVN_NETWORK_" + strings.ToUpper(wantKey)
		if o.EnvVar() != wantEnv {
			t.Errorf("option %q: EnvVar() = %q, want %q", o.Flag, o.EnvVar(), wantEnv)
		}
	}
}

// TestConfigOptionsDefaultsMatchLoadConfig proves the registry's defaults are
// the ones actually applied: loading with no file, no env and no flags must
// reproduce every applyDefault.
func TestConfigOptionsDefaultsMatchLoadConfig(t *testing.T) {
	var want Config
	for _, o := range configOptions() {
		if o.applyDefault != nil {
			o.applyDefault(&want)
		}
	}

	got, err := loadConfig(fullModeArgs())
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	// The OVN remotes come from fullModeArgs, and these three are derived by
	// validateMode/validateConfig rather than declared as options.
	want.OVNSBRemote = got.OVNSBRemote
	want.OVNNBRemote = got.OVNNBRemote
	want.NetworkFilters = got.NetworkFilters
	want.PortForwardEnabled = got.PortForwardEnabled
	want.PortForwardOnly = got.PortForwardOnly
	want.VethProviderIP = got.VethProviderIP

	if got.BridgeDev != want.BridgeDev || got.VRFName != want.VRFName ||
		got.ReconcileInterval != want.ReconcileInterval ||
		got.DrainSettleDelay != want.DrainSettleDelay ||
		got.CleanupOnShutdown != want.CleanupOnShutdown ||
		got.PortForwardCTZone != want.PortForwardCTZone {
		t.Errorf("loadConfig defaults diverge from the registry:\n got %+v\nwant %+v", got, want)
	}
}

// writeTestTLSFiles generates a self-signed ECDSA P-256 certificate and its
// private key, writes both as PEM into a temp dir, and returns the two paths.
// The certificate file doubles as a CA input for ovnTLSConfig: AppendCertsFromPEM
// only parses the PEM (no chain verification) and tls.LoadX509KeyPair does not
// verify the leaf against a CA either, so one self-signed cert satisfies both.
func writeTestTLSFiles(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	return writeTestTLSFilesNotAfter(t, time.Now().Add(time.Hour))
}

// writeTestTLSFilesNotAfter is writeTestTLSFiles with a caller-chosen NotAfter,
// so a test can produce a certificate that is already past its validity.
func writeTestTLSFilesNotAfter(t *testing.T, notAfter time.Time) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ovn-network-agent-test"},
		NotBefore:             notAfter.Add(-24 * time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	writeTestPEM(t, certPath, "CERTIFICATE", der)
	writeTestPEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func writeTestPEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	buf := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestOVNTLSConfig(t *testing.T) {
	certPath, keyPath := writeTestTLSFiles(t)

	garbagePath := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(garbagePath, []byte("not a pem file\n"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	missingPath := filepath.Join(t.TempDir(), "does-not-exist.pem")

	// A valid key that a config-management run left group-readable.
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	looseKeyPath := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(looseKeyPath, keyPEM, 0o640); err != nil {
		t.Fatalf("write group-readable key: %v", err)
	}
	// open(2) masks the mode with the process umask, so under umask 077 the
	// file would land at 0600 and this case would stop testing anything.
	if err := os.Chmod(looseKeyPath, 0o640); err != nil {
		t.Fatalf("chmod group-readable key: %v", err)
	}

	tests := []struct {
		name             string
		ca, cert, key    string
		wantNil          bool     // expect (nil, nil)
		wantErr          []string // substrings the error must contain; empty = expect success
		wantRootCAs      bool
		wantCertificates int
	}{
		{
			name:    "all empty",
			wantNil: true,
		},
		{
			name:             "ca only",
			ca:               certPath,
			wantRootCAs:      true,
			wantCertificates: 0,
		},
		{
			name:             "ca cert key",
			ca:               certPath,
			cert:             certPath,
			key:              keyPath,
			wantRootCAs:      true,
			wantCertificates: 1,
		},
		{
			// Mutual TLS against servers whose certificates chain to the
			// system trust store: legal, and the only quadrant where
			// RootCAs must stay nil while a client certificate is loaded.
			name:             "cert key without ca",
			cert:             certPath,
			key:              keyPath,
			wantRootCAs:      false,
			wantCertificates: 1,
		},
		{
			name:    "cert without key",
			cert:    certPath,
			wantErr: []string{"ovn-ssl-cert", "ovn-ssl-key"},
		},
		{
			name:    "group readable key",
			cert:    certPath,
			key:     looseKeyPath,
			wantErr: []string{"ovn-ssl-key", "0640", "group- or world-accessible"},
		},
		{
			name:    "nonexistent key",
			cert:    certPath,
			key:     missingPath,
			wantErr: []string{"stat ovn-ssl-key", missingPath},
		},
		{
			name:    "key without cert",
			key:     keyPath,
			wantErr: []string{"ovn-ssl-cert", "ovn-ssl-key"},
		},
		{
			name:    "nonexistent ca",
			ca:      missingPath,
			wantErr: []string{"ovn-ssl-ca", missingPath},
		},
		{
			name:    "ca without pem certificates",
			ca:      garbagePath,
			wantErr: []string{"no PEM certificates found"},
		},
		{
			name:    "garbage cert key pair",
			cert:    garbagePath,
			key:     garbagePath,
			wantErr: []string{"ovn-ssl-cert", "ovn-ssl-key"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := ovnTLSConfig(tt.ca, tt.cert, tt.key)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("ovnTLSConfig() error = nil, want error")
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ovnTLSConfig() error = %v, want nil", err)
			}
			if tt.wantNil {
				if tc != nil {
					t.Fatalf("ovnTLSConfig() = %+v, want nil", tc)
				}
				return
			}
			if tc == nil {
				t.Fatal("ovnTLSConfig() = nil, want non-nil")
			}
			if tc.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %d, want %d (TLS 1.2)", tc.MinVersion, tls.VersionTLS12)
			}
			if (tc.RootCAs != nil) != tt.wantRootCAs {
				t.Errorf("RootCAs set = %v, want %v", tc.RootCAs != nil, tt.wantRootCAs)
			}
			if len(tc.Certificates) != tt.wantCertificates {
				t.Errorf("Certificates = %d, want %d", len(tc.Certificates), tt.wantCertificates)
			}
		})
	}
}

// TestOVNTLSConfigWarnsOnExpiredCert pins the expiry warning. LoadX509KeyPair
// validates the key against the certificate but ignores NotBefore/NotAfter, so
// an expired client certificate otherwise loads cleanly, passes --check-config,
// and only fails at the next handshake — long after the PEMs were rotated.
func TestOVNTLSConfigWarnsOnExpiredCert(t *testing.T) {
	const warning = "ovn-ssl-cert has expired"

	t.Run("expired cert warns but still loads", func(t *testing.T) {
		buf := captureSlog(t)
		certPath, keyPath := writeTestTLSFilesNotAfter(t, time.Now().Add(-time.Hour))

		tc, err := ovnTLSConfig("", certPath, keyPath)
		if err != nil {
			t.Fatalf("ovnTLSConfig() error = %v, want nil", err)
		}
		if len(tc.Certificates) != 1 {
			t.Errorf("Certificates = %d, want 1 (an expired cert must warn, not be dropped)", len(tc.Certificates))
		}
		if !strings.Contains(buf.String(), warning) {
			t.Errorf("warnings %q do not contain %q", buf.String(), warning)
		}
	})

	t.Run("valid cert does not warn", func(t *testing.T) {
		buf := captureSlog(t)
		certPath, keyPath := writeTestTLSFiles(t)

		if _, err := ovnTLSConfig("", certPath, keyPath); err != nil {
			t.Fatalf("ovnTLSConfig() error = %v, want nil", err)
		}
		if strings.Contains(buf.String(), warning) {
			t.Errorf("warnings %q unexpectedly contain %q", buf.String(), warning)
		}
	})
}

// TestValidateConfigWarnsOnClientCertWithoutCA covers the quadrant where the
// agent presents a client certificate but pins no CA. It is the most dangerous
// of the partial TLS setups — server certificates fall back to the host's
// system root store, so any publicly trusted CA can impersonate the databases —
// and the least likely to be noticed, because the connection comes up and works.
func TestValidateConfigWarnsOnClientCertWithoutCA(t *testing.T) {
	certPath, keyPath := writeTestTLSFiles(t)
	// Substring unique to this warning: the "no TLS material at all" warning
	// also names the system trust store.
	const warning = "set ovn-ssl-ca to pin"

	tests := []struct {
		name        string
		withCA      bool
		ssl         bool
		wantWarning bool
	}{
		{
			name:        "cert and key without ca over ssl",
			ssl:         true,
			wantWarning: true,
		},
		{
			// The CA pin is what makes the difference; with it the same
			// remotes are fully verified.
			name:   "ca cert and key over ssl",
			withCA: true,
			ssl:    true,
		},
		{
			// Inert TLS material over cleartext remotes: the operator gets
			// the "no effect" warning, not a trust-store one.
			name: "cert and key without ca over tcp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureSlog(t)
			cfg := Config{
				VethNexthop:       "169.254.0.1",
				VRFName:           "vrf-provider",
				ReconcileInterval: 60 * time.Second,
				OVNSSLCert:        certPath,
				OVNSSLKey:         keyPath,
			}
			if tt.withCA {
				cfg.OVNSSLCA = certPath
			}
			if tt.ssl {
				cfg.OVNSBRemote = "ssl:10.0.0.1:6642"
				cfg.OVNNBRemote = "ssl:10.0.0.1:6641"
			} else {
				cfg.OVNSBRemote = "tcp:10.0.0.1:6642"
				cfg.OVNNBRemote = "tcp:10.0.0.1:6641"
			}

			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("validateConfig() error = %v, want nil", err)
			}
			if got := strings.Contains(buf.String(), warning); got != tt.wantWarning {
				t.Errorf("warning %q present = %v, want %v; warnings: %q", warning, got, tt.wantWarning, buf.String())
			}
		})
	}
}

func TestValidateConfigDerivesOVNTLS(t *testing.T) {
	certPath, keyPath := writeTestTLSFiles(t)

	baseConfig := func() Config {
		return Config{
			VethNexthop:       "169.254.0.1",
			VRFName:           "vrf-provider",
			ReconcileInterval: 60 * time.Second,
		}
	}

	t.Run("paths set", func(t *testing.T) {
		cfg := baseConfig()
		cfg.OVNSSLCA = certPath
		cfg.OVNSSLCert = certPath
		cfg.OVNSSLKey = keyPath
		if err := validateConfig(&cfg); err != nil {
			t.Fatalf("validateConfig: %v", err)
		}
		if cfg.OVNTLS == nil {
			t.Fatal("OVNTLS = nil, want non-nil after validateConfig")
		}
	})

	t.Run("paths unset", func(t *testing.T) {
		cfg := baseConfig()
		if err := validateConfig(&cfg); err != nil {
			t.Fatalf("validateConfig: %v", err)
		}
		if cfg.OVNTLS != nil {
			t.Fatalf("OVNTLS = %+v, want nil when no ovn-ssl-* option is set", cfg.OVNTLS)
		}
	})
}

func TestValidateConfigRejectsMixedSSLRemotes(t *testing.T) {
	tests := []struct {
		name     string
		sbRemote string
		nbRemote string
		wantErr  string // substring the error must name; empty = expect no error
	}{
		{
			name:     "mixed ssl and tcp in SB list",
			sbRemote: "ssl:10.0.0.1:6642,tcp:10.0.0.2:6642",
			wantErr:  "ovn-sb-remote",
		},
		{
			name:     "mixed ssl and tcp in NB list",
			nbRemote: "ssl:10.0.0.1:6641,tcp:10.0.0.2:6641",
			wantErr:  "ovn-nb-remote",
		},
		{
			name:     "all ssl in both lists",
			sbRemote: "ssl:10.0.0.1:6642,ssl:10.0.0.2:6642",
			nbRemote: "ssl:10.0.0.1:6641,ssl:10.0.0.2:6641",
		},
		{
			name:     "ssl NB with tcp SB",
			sbRemote: "tcp:10.0.0.1:6642",
			nbRemote: "ssl:10.0.0.1:6641",
		},
		{
			name:     "ssl remotes without ca (warn only)",
			sbRemote: "ssl:10.0.0.1:6642",
			nbRemote: "ssl:10.0.0.1:6641",
		},
		{
			// libovsdb lowercases the scheme, so "SSL:" dials TLS and
			// "tcp:" does not — the list still downgrades on failover.
			name:     "mixed uppercase SSL and tcp in NB list",
			nbRemote: "SSL:10.0.0.1:6641,tcp:10.0.0.2:6641",
			wantErr:  "ovn-nb-remote",
		},
		{
			name:     "uppercase SSL in both lists",
			sbRemote: "SSL:10.0.0.1:6642",
			nbRemote: "SSL:10.0.0.1:6641",
		},
		{
			name:     "unsupported scheme in SB list",
			sbRemote: "sl:10.0.0.1:6642",
			nbRemote: "tcp:10.0.0.1:6641",
			wantErr:  "unsupported endpoint scheme",
		},
		{
			name:     "endpoint without scheme in NB list",
			sbRemote: "tcp:10.0.0.1:6642",
			nbRemote: "localhost",
			wantErr:  "has no scheme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				VethNexthop:       "169.254.0.1",
				VRFName:           "vrf-provider",
				ReconcileInterval: 60 * time.Second,
				OVNSBRemote:       tt.sbRemote,
				OVNNBRemote:       tt.nbRemote,
			}
			err := validateConfig(&cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateConfig() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateConfig() error = nil, want error naming %s", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not name %s", err, tt.wantErr)
			}
		})
	}
}

// TestValidateConfigOVNSchemeWarnings pins the warnings that depend on how
// validateConfig classifies each remote's scheme. They are the only signal an
// operator gets for a half-configured TLS setup, so a misclassified scheme
// shows up here as a missing — or, worse, a factually wrong — warning.
func TestValidateConfigOVNSchemeWarnings(t *testing.T) {
	certPath, keyPath := writeTestTLSFiles(t)

	tests := []struct {
		name       string
		sbRemote   string
		nbRemote   string
		withTLS    bool
		wantLog    []string // substrings the warnings must contain
		wantNotLog []string // substrings the warnings must not contain
	}{
		{
			// Both remotes dial TLS in libovsdb, so the TLS material is
			// load-bearing: telling the operator it "has no effect" would
			// invite them to delete the CA pin.
			name:       "uppercase SSL remotes with TLS material",
			sbRemote:   "SSL:10.0.0.1:6642",
			nbRemote:   "SSL:10.0.0.1:6641",
			withTLS:    true,
			wantNotLog: []string{"have no effect", "mixed schemes"},
		},
		{
			name:     "TLS NB with cleartext SB",
			sbRemote: "tcp:10.0.0.1:6642",
			nbRemote: "ssl:10.0.0.1:6641",
			withTLS:  true,
			wantLog:  []string{"mixed schemes"},
		},
		{
			name:       "both remotes cleartext",
			sbRemote:   "tcp:10.0.0.1:6642",
			nbRemote:   "tcp:10.0.0.1:6641",
			wantNotLog: []string{"mixed schemes"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureSlog(t)
			cfg := Config{
				VethNexthop:       "169.254.0.1",
				VRFName:           "vrf-provider",
				ReconcileInterval: 60 * time.Second,
				OVNSBRemote:       tt.sbRemote,
				OVNNBRemote:       tt.nbRemote,
			}
			if tt.withTLS {
				cfg.OVNSSLCA = certPath
				cfg.OVNSSLCert = certPath
				cfg.OVNSSLKey = keyPath
			}
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("validateConfig() error = %v, want nil", err)
			}
			for _, want := range tt.wantLog {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("warnings %q do not contain %q", buf.String(), want)
				}
			}
			for _, notWant := range tt.wantNotLog {
				if strings.Contains(buf.String(), notWant) {
					t.Errorf("warnings %q unexpectedly contain %q", buf.String(), notWant)
				}
			}
		})
	}
}

func TestLoadConfigOVNSSLPrecedence(t *testing.T) {
	caFile, _ := writeTestTLSFiles(t)
	caEnv, _ := writeTestTLSFiles(t)
	caFlag, _ := writeTestTLSFiles(t)

	content := fmt.Sprintf(`
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
veth_leak_enabled: false
ovn_ssl_ca: %q
`, caFile)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("OVN_NETWORK_OVN_SSL_CA", caEnv)

	cfg, err := loadConfig([]string{"--config", path, "--ovn-ssl-ca", caFlag})
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.OVNSSLCA != caFlag {
		t.Errorf("OVNSSLCA = %q, want %q (flag > env > file)", cfg.OVNSSLCA, caFlag)
	}
	if cfg.OVNTLS == nil {
		t.Error("OVNTLS = nil, want non-nil (ovn-ssl-ca resolves to a real cert)")
	}
}
