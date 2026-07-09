package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	content := `
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
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	fc, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error: %v", err)
	}

	if fc.OVNSBRemote != "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", fc.OVNSBRemote, "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642")
	}
	if fc.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", fc.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if fc.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", fc.BridgeDev, "br-provider")
	}
	if fc.VRFName != "vrf-test" {
		t.Errorf("VRFName = %q, want %q", fc.VRFName, "vrf-test")
	}
	if fc.VethNexthop != "169.254.0.2" {
		t.Errorf("VethNexthop = %q, want %q", fc.VethNexthop, "169.254.0.2")
	}
	if len(fc.NetworkCIDR) != 1 || fc.NetworkCIDR[0] != "192.0.2.0/24" {
		t.Errorf("NetworkCIDR = %v, want [192.0.2.0/24]", fc.NetworkCIDR)
	}
	if fc.GatewayPort != "cr-lrp-abc123" {
		t.Errorf("GatewayPort = %q, want %q", fc.GatewayPort, "cr-lrp-abc123")
	}
	if fc.ReconcileInterval != "30s" {
		t.Errorf("ReconcileInterval = %q, want %q", fc.ReconcileInterval, "30s")
	}
	if fc.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q", fc.LogLevel, "debug")
	}
	if fc.CleanupOnShutdown == nil || *fc.CleanupOnShutdown != false {
		t.Errorf("CleanupOnShutdown = %v, want false", fc.CleanupOnShutdown)
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

func TestReadConfigFileNetworkCIDRList(t *testing.T) {
	content := `
ovn_sb_remote: "tcp:10.0.0.1:6642"
ovn_nb_remote: "tcp:10.0.0.1:6641"
network_cidr:
  - "192.0.2.0/24"
  - "198.51.100.0/24"
`
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}

	fc, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error: %v", err)
	}

	if len(fc.NetworkCIDR) != 2 {
		t.Fatalf("NetworkCIDR length = %d, want 2", len(fc.NetworkCIDR))
	}
	if fc.NetworkCIDR[0] != "192.0.2.0/24" || fc.NetworkCIDR[1] != "198.51.100.0/24" {
		t.Errorf("NetworkCIDR = %v, want [192.0.2.0/24 198.51.100.0/24]", fc.NetworkCIDR)
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
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0644); err != nil {
				t.Fatalf("write test config: %v", err)
			}

			// Must not panic on a mapping node — the function decodes the
			// value into a []string in the non-scalar branch, and yaml.v3
			// returns an error from Decode rather than panicking. Catch any
			// panic explicitly so a future refactor that drops the safe
			// Decode path is flagged here.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("UnmarshalYAML panicked on mapping input: %v", r)
				}
			}()

			fc, err := readConfigFile(path)
			if err == nil {
				t.Fatalf("expected decode error for mapping input, got success with NetworkCIDR=%v", fc.NetworkCIDR)
			}
			if errStr := err.Error(); errStr == "" {
				t.Errorf("decode error message is empty")
			}
		})
	}
}

func TestApplyEnvConfigMultipleCIDRs(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_NETWORK_CIDR", "10.0.0.0/24,172.16.0.0/12")
	applyEnvConfig(&cfg)

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

	fc := configFile{
		OVNSBRemote:       "tcp:10.0.0.1:6642",
		OVNNBRemote:       "tcp:10.0.0.1:6641",
		BridgeDev:         "br-provider",
		ReconcileInterval: "30s",
	}

	applyFileConfig(&cfg, &fc)

	if cfg.OVNSBRemote != "tcp:10.0.0.1:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.1:6642")
	}
	if cfg.OVNNBRemote != "tcp:10.0.0.1:6641" {
		t.Errorf("OVNNBRemote = %q, want %q", cfg.OVNNBRemote, "tcp:10.0.0.1:6641")
	}
	if cfg.BridgeDev != "br-provider" {
		t.Errorf("BridgeDev = %q, want %q", cfg.BridgeDev, "br-provider")
	}
	// Unchanged fields keep defaults.
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

	fc := configFile{} // All empty.

	applyFileConfig(&cfg, &fc)

	if cfg.BridgeDev != "br-ex" {
		t.Errorf("BridgeDev = %q, want %q (should not be overridden)", cfg.BridgeDev, "br-ex")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want %q (should not be overridden)", cfg.LogLevel, "info")
	}
}

func TestApplyEnvConfig(t *testing.T) {
	cfg := Config{
		BridgeDev: "br-ex",
		LogLevel:  "info",
	}

	t.Setenv("OVN_NETWORK_OVN_SB_REMOTE", "tcp:10.0.0.99:6642")
	t.Setenv("OVN_NETWORK_LOG_LEVEL", "debug")
	t.Setenv("OVN_NETWORK_RECONCILE_INTERVAL", "5m")
	t.Setenv("OVN_NETWORK_NETWORK_CIDR", "10.0.0.0/24")
	t.Setenv("OVN_NETWORK_GATEWAY_PORT", "cr-lrp-test")

	applyEnvConfig(&cfg)

	if cfg.OVNSBRemote != "tcp:10.0.0.99:6642" {
		t.Errorf("OVNSBRemote = %q, want %q", cfg.OVNSBRemote, "tcp:10.0.0.99:6642")
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
	cfg := Config{
		ReconcileInterval: 60 * time.Second,
	}

	t.Setenv("OVN_NETWORK_RECONCILE_INTERVAL", "notaduration")

	applyEnvConfig(&cfg)

	if cfg.ReconcileInterval != 60*time.Second {
		t.Errorf("ReconcileInterval = %v, want %v (should not be overridden by invalid value)", cfg.ReconcileInterval, 60*time.Second)
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
	// BFD: the check is on by default, both manage flags are off.
	if !cfg.BFDCheckEnabled {
		t.Error("BFDCheckEnabled should be true by default")
	}
	if cfg.BFDCheckMaxDetect != time.Second {
		t.Errorf("BFDCheckMaxDetect = %v, want %v", cfg.BFDCheckMaxDetect, time.Second)
	}
	if cfg.OVNBFDManage {
		t.Error("OVNBFDManage should be false by default")
	}
	if cfg.FRRBFDManage {
		t.Error("FRRBFDManage should be false by default")
	}
	if cfg.OVNBFDMinRxMs != 150 || cfg.OVNBFDMinTxMs != 150 {
		t.Errorf("OVN BFD timers = %d/%d, want 150/150", cfg.OVNBFDMinRxMs, cfg.OVNBFDMinTxMs)
	}
	if cfg.FRRBFDMinRxMs != 150 || cfg.FRRBFDMinTxMs != 150 {
		t.Errorf("FRR BFD timers = %d/%d, want 150/150", cfg.FRRBFDMinRxMs, cfg.FRRBFDMinTxMs)
	}
	if cfg.FRRBFDMultiplier != 3 {
		t.Errorf("FRRBFDMultiplier = %d, want 3", cfg.FRRBFDMultiplier)
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

func TestApplyEnvConfigDryRun(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_DRY_RUN", "true")
	applyEnvConfig(&cfg)
	if !cfg.DryRun {
		t.Error("DryRun should be true when OVN_NETWORK_DRY_RUN=true")
	}
}

func TestApplyFileConfigDryRun(t *testing.T) {
	cfg := Config{}
	dryRun := true
	fc := configFile{DryRun: &dryRun}
	applyFileConfig(&cfg, &fc)
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
	applyEnvConfig(&cfg)
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when OVN_NETWORK_CLEANUP_ON_SHUTDOWN=false")
	}
}

func TestApplyEnvConfigCleanupOnShutdownZero(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	t.Setenv("OVN_NETWORK_CLEANUP_ON_SHUTDOWN", "0")
	applyEnvConfig(&cfg)
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when OVN_NETWORK_CLEANUP_ON_SHUTDOWN=0")
	}
}

func TestApplyFileConfigCleanupOnShutdown(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	cleanup := false
	fc := configFile{CleanupOnShutdown: &cleanup}
	applyFileConfig(&cfg, &fc)
	if cfg.CleanupOnShutdown {
		t.Error("CleanupOnShutdown should be false when set to false in config file")
	}
}

func TestApplyFileConfigCleanupOnShutdownNil(t *testing.T) {
	cfg := Config{CleanupOnShutdown: true}
	fc := configFile{} // CleanupOnShutdown is nil
	applyFileConfig(&cfg, &fc)
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
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider"},
			false,
		},
		{
			"valid with single CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"10.0.0.0/24"}},
			false,
		},
		{
			"valid with multiple CIDRs",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"10.0.0.0/24", "172.16.0.0/12"}},
			false,
		},
		{
			"invalid nexthop",
			Config{VethNexthop: "bad", VRFName: "vrf-provider"},
			true,
		},
		{
			"invalid VRF name",
			Config{VethNexthop: "169.254.0.1", VRFName: "bad name"},
			true,
		},
		{
			"invalid CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"bad"}},
			true,
		},
		{
			"one valid one invalid CIDR",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", NetworkCIDRs: []string{"10.0.0.0/24", "bad"}},
			true,
		},
		{
			"valid route table ID",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 100},
			false,
		},
		{
			"route table ID zero (main table)",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 0},
			false,
		},
		{
			"route table ID max",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 252},
			false,
		},
		{
			"route table ID too high",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: 253},
			true,
		},
		{
			"route table ID negative",
			Config{VethNexthop: "169.254.0.1", VRFName: "vrf-provider", RouteTableID: -1},
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
	applyEnvConfig(&cfg)

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
	applyEnvConfig(&cfg)

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
	enabled := false
	tableID := 201
	prio := 3000
	fc := configFile{
		VethLeakEnabled:      &enabled,
		VethProviderIP:       "169.254.0.5",
		VethLeakTableID:      &tableID,
		VethLeakRulePriority: &prio,
	}
	applyFileConfig(&cfg, &fc)

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
	applyEnvConfig(&cfg)
	if cfg.StaleChassisGracePeriod != 3*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 3*time.Minute)
	}
}

func TestApplyFileConfigStaleChassisGracePeriod(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	fc := configFile{StaleChassisGracePeriod: "2m"}
	applyFileConfig(&cfg, &fc)
	if cfg.StaleChassisGracePeriod != 2*time.Minute {
		t.Errorf("StaleChassisGracePeriod = %v, want %v", cfg.StaleChassisGracePeriod, 2*time.Minute)
	}
}

func TestApplyFileConfigStaleChassisGracePeriodEmpty(t *testing.T) {
	cfg := Config{StaleChassisGracePeriod: 5 * time.Minute}
	fc := configFile{}
	applyFileConfig(&cfg, &fc)
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
	if cfg.DrainSettleDelay != 3*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 3*time.Second)
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
	applyEnvConfig(&cfg)
	if cfg.DrainSettleDelay != 5*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 5*time.Second)
	}
}

func TestApplyFileConfigDrainSettleDelay(t *testing.T) {
	cfg := Config{DrainSettleDelay: 3 * time.Second}
	fc := configFile{DrainSettleDelay: "10s"}
	applyFileConfig(&cfg, &fc)
	if cfg.DrainSettleDelay != 10*time.Second {
		t.Errorf("DrainSettleDelay = %v, want %v", cfg.DrainSettleDelay, 10*time.Second)
	}
}

func TestApplyFileConfigDrainSettleDelayEmpty(t *testing.T) {
	cfg := Config{DrainSettleDelay: 3 * time.Second}
	fc := configFile{}
	applyFileConfig(&cfg, &fc)
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
		VethNexthop:      "169.254.0.1",
		VRFName:          "vrf-provider",
		DrainSettleDelay: -1 * time.Second,
	}
	err := validateConfig(&cfg)
	if err == nil {
		t.Error("expected error for negative drain-settle-delay")
	}
}

func TestApplyEnvConfigFRRPrefixList(t *testing.T) {
	cfg := Config{}
	t.Setenv("OVN_NETWORK_FRR_PREFIX_LIST", "MY-LIST")
	applyEnvConfig(&cfg)
	if cfg.FRRPrefixList != "MY-LIST" {
		t.Errorf("FRRPrefixList = %q, want %q", cfg.FRRPrefixList, "MY-LIST")
	}
}

func TestApplyFileConfigFRRPrefixList(t *testing.T) {
	cfg := Config{}
	fc := configFile{FRRPrefixList: "FILE-LIST"}
	applyFileConfig(&cfg, &fc)
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

	applyEnvConfig(&cfg)

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
	tableID := 205
	fc := configFile{
		PortForwardDev:     "loopback1",
		PortForwardTableID: &tableID,
		PortForwards: []PortForwardVIP{
			{VIP: "198.51.100.10", Rules: []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"}}},
		},
	}

	applyFileConfig(&cfg, &fc)

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("PortForwardDev = %q, want %q", cfg.PortForwardDev, "loopback1")
	}
	if cfg.PortForwardTableID != 205 {
		t.Errorf("PortForwardTableID = %d, want %d", cfg.PortForwardTableID, 205)
	}
	if len(cfg.PortForwards) != 1 {
		t.Errorf("len(PortForwards) = %d, want 1", len(cfg.PortForwards))
	}
}

func TestApplyFileConfigPortForwardEmpty(t *testing.T) {
	cfg := Config{
		PortForwardDev:     "loopback1",
		PortForwardTableID: 201,
	}
	fc := configFile{} // no port forward fields set

	applyFileConfig(&cfg, &fc)

	if cfg.PortForwardDev != "loopback1" {
		t.Errorf("PortForwardDev should remain %q, got %q", "loopback1", cfg.PortForwardDev)
	}
	if cfg.PortForwardTableID != 201 {
		t.Errorf("PortForwardTableID should remain %d, got %d", 201, cfg.PortForwardTableID)
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
		VethNexthop:     "255.255.255.255",
		VethLeakEnabled: true,
		VethLeakTableID: 200,
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
	applyEnvConfig(&cfg)
	if cfg.MetricsListen != "0.0.0.0:9273" {
		t.Errorf("MetricsListen = %q, want %q", cfg.MetricsListen, "0.0.0.0:9273")
	}
}

func TestApplyFileConfigMetricsListen(t *testing.T) {
	cfg := Config{}
	listen := "127.0.0.1:9273"
	fc := configFile{MetricsListen: listen}
	applyFileConfig(&cfg, &fc)
	if cfg.MetricsListen != listen {
		t.Errorf("MetricsListen = %q, want %q", cfg.MetricsListen, listen)
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

// =============================================================================
// BFD failover detection options
// =============================================================================

func TestLoadConfigBFDCLIFlags(t *testing.T) {
	cfg, err := loadConfig(fullModeArgs(
		"--bfd-check-enabled=false",
		"--bfd-check-max-detect", "500ms",
		"--ovn-bfd-manage",
		"--ovn-bfd-min-rx-ms", "100",
		"--ovn-bfd-min-tx-ms", "120",
		"--frr-bfd-manage",
		"--frr-bfd-min-rx-ms", "200",
		"--frr-bfd-min-tx-ms", "250",
		"--frr-bfd-multiplier", "5",
	))
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.BFDCheckEnabled {
		t.Error("BFDCheckEnabled should be false")
	}
	if cfg.BFDCheckMaxDetect != 500*time.Millisecond {
		t.Errorf("BFDCheckMaxDetect = %v, want 500ms", cfg.BFDCheckMaxDetect)
	}
	if !cfg.OVNBFDManage || !cfg.FRRBFDManage {
		t.Error("both manage flags should be true")
	}
	if cfg.OVNBFDMinRxMs != 100 || cfg.OVNBFDMinTxMs != 120 {
		t.Errorf("OVN BFD timers = %d/%d, want 100/120", cfg.OVNBFDMinRxMs, cfg.OVNBFDMinTxMs)
	}
	if cfg.FRRBFDMinRxMs != 200 || cfg.FRRBFDMinTxMs != 250 {
		t.Errorf("FRR BFD timers = %d/%d, want 200/250", cfg.FRRBFDMinRxMs, cfg.FRRBFDMinTxMs)
	}
	if cfg.FRRBFDMultiplier != 5 {
		t.Errorf("FRRBFDMultiplier = %d, want 5", cfg.FRRBFDMultiplier)
	}
}

// The manage flags are independent: enabling one must not enable the other.
func TestLoadConfigBFDManageFlagsIndependent(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantOVN bool
		wantFRR bool
	}{
		{"both off by default", nil, false, false},
		{"only OVN", []string{"--ovn-bfd-manage"}, true, false},
		{"only FRR", []string{"--frr-bfd-manage"}, false, true},
		{"both", []string{"--ovn-bfd-manage", "--frr-bfd-manage"}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(fullModeArgs(tt.args...))
			if err != nil {
				t.Fatalf("loadConfig() error: %v", err)
			}
			if cfg.OVNBFDManage != tt.wantOVN {
				t.Errorf("OVNBFDManage = %v, want %v", cfg.OVNBFDManage, tt.wantOVN)
			}
			if cfg.FRRBFDManage != tt.wantFRR {
				t.Errorf("FRRBFDManage = %v, want %v", cfg.FRRBFDManage, tt.wantFRR)
			}
		})
	}
}

func TestApplyEnvConfigBFD(t *testing.T) {
	cfg := Config{BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second}
	t.Setenv("OVN_NETWORK_BFD_CHECK_ENABLED", "false")
	t.Setenv("OVN_NETWORK_BFD_CHECK_MAX_DETECT", "750ms")
	t.Setenv("OVN_NETWORK_OVN_BFD_MANAGE", "true")
	t.Setenv("OVN_NETWORK_OVN_BFD_MIN_RX_MS", "100")
	t.Setenv("OVN_NETWORK_OVN_BFD_MIN_TX_MS", "110")
	t.Setenv("OVN_NETWORK_FRR_BFD_MANAGE", "1")
	t.Setenv("OVN_NETWORK_FRR_BFD_MIN_RX_MS", "200")
	t.Setenv("OVN_NETWORK_FRR_BFD_MIN_TX_MS", "210")
	t.Setenv("OVN_NETWORK_FRR_BFD_MULTIPLIER", "4")
	applyEnvConfig(&cfg)

	if cfg.BFDCheckEnabled {
		t.Error("BFDCheckEnabled should be false")
	}
	if cfg.BFDCheckMaxDetect != 750*time.Millisecond {
		t.Errorf("BFDCheckMaxDetect = %v, want 750ms", cfg.BFDCheckMaxDetect)
	}
	if !cfg.OVNBFDManage || !cfg.FRRBFDManage {
		t.Error("both manage flags should be true")
	}
	if cfg.OVNBFDMinRxMs != 100 || cfg.OVNBFDMinTxMs != 110 {
		t.Errorf("OVN BFD timers = %d/%d, want 100/110", cfg.OVNBFDMinRxMs, cfg.OVNBFDMinTxMs)
	}
	if cfg.FRRBFDMinRxMs != 200 || cfg.FRRBFDMinTxMs != 210 {
		t.Errorf("FRR BFD timers = %d/%d, want 200/210", cfg.FRRBFDMinRxMs, cfg.FRRBFDMinTxMs)
	}
	if cfg.FRRBFDMultiplier != 4 {
		t.Errorf("FRRBFDMultiplier = %d, want 4", cfg.FRRBFDMultiplier)
	}
}

// Malformed env values must be ignored, leaving the prior value intact —
// a typo must not silently reconfigure the BFD timers.
func TestApplyEnvConfigBFDInvalidValues(t *testing.T) {
	cfg := Config{BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second, OVNBFDMinRxMs: 150, FRRBFDMultiplier: 3}
	t.Setenv("OVN_NETWORK_BFD_CHECK_ENABLED", "yes-please") // not "0"/"false"
	t.Setenv("OVN_NETWORK_BFD_CHECK_MAX_DETECT", "soon")
	t.Setenv("OVN_NETWORK_OVN_BFD_MANAGE", "maybe") // not "1"/"true"
	t.Setenv("OVN_NETWORK_OVN_BFD_MIN_RX_MS", "fast")
	t.Setenv("OVN_NETWORK_FRR_BFD_MULTIPLIER", "many")
	applyEnvConfig(&cfg)

	if !cfg.BFDCheckEnabled {
		t.Error("BFDCheckEnabled should stay true for an unrecognised value")
	}
	if cfg.BFDCheckMaxDetect != time.Second {
		t.Errorf("BFDCheckMaxDetect = %v, want 1s (invalid value ignored)", cfg.BFDCheckMaxDetect)
	}
	if cfg.OVNBFDManage {
		t.Error("OVNBFDManage should stay false for an unrecognised value")
	}
	if cfg.OVNBFDMinRxMs != 150 {
		t.Errorf("OVNBFDMinRxMs = %d, want 150 (invalid value ignored)", cfg.OVNBFDMinRxMs)
	}
	if cfg.FRRBFDMultiplier != 3 {
		t.Errorf("FRRBFDMultiplier = %d, want 3 (invalid value ignored)", cfg.FRRBFDMultiplier)
	}
}

func TestApplyFileConfigBFD(t *testing.T) {
	cfg := Config{BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second, OVNBFDMinRxMs: 150, FRRBFDMultiplier: 3}
	checkEnabled := false
	ovnManage := true
	frrManage := true
	minRx := 100
	mult := 5
	fc := configFile{
		BFDCheckEnabled:   &checkEnabled,
		BFDCheckMaxDetect: "450ms",
		OVNBFDManage:      &ovnManage,
		OVNBFDMinRxMs:     &minRx,
		FRRBFDManage:      &frrManage,
		FRRBFDMultiplier:  &mult,
	}
	applyFileConfig(&cfg, &fc)

	if cfg.BFDCheckEnabled {
		t.Error("BFDCheckEnabled should be false")
	}
	if cfg.BFDCheckMaxDetect != 450*time.Millisecond {
		t.Errorf("BFDCheckMaxDetect = %v, want 450ms", cfg.BFDCheckMaxDetect)
	}
	if !cfg.OVNBFDManage || !cfg.FRRBFDManage {
		t.Error("both manage flags should be true")
	}
	if cfg.OVNBFDMinRxMs != 100 {
		t.Errorf("OVNBFDMinRxMs = %d, want 100", cfg.OVNBFDMinRxMs)
	}
	if cfg.FRRBFDMultiplier != 5 {
		t.Errorf("FRRBFDMultiplier = %d, want 5", cfg.FRRBFDMultiplier)
	}
	// Unset pointers must not clobber the existing values.
	if cfg.OVNBFDMinTxMs != 0 {
		t.Errorf("OVNBFDMinTxMs = %d, want 0 (untouched)", cfg.OVNBFDMinTxMs)
	}
}

func TestApplyFileConfigBFDInvalidDuration(t *testing.T) {
	cfg := Config{BFDCheckMaxDetect: time.Second}
	applyFileConfig(&cfg, &configFile{BFDCheckMaxDetect: "not-a-duration"})
	if cfg.BFDCheckMaxDetect != time.Second {
		t.Errorf("BFDCheckMaxDetect = %v, want 1s (invalid value ignored)", cfg.BFDCheckMaxDetect)
	}
}

// BFD validation is gated on each feature's enable flag: out-of-range values
// for a disabled feature must not prevent the agent from starting.
func TestValidateConfigBFD(t *testing.T) {
	base := func() Config {
		return Config{
			VethNexthop:       "169.254.0.1",
			VRFName:           "vrf-provider",
			BFDCheckEnabled:   true,
			BFDCheckMaxDetect: time.Second,
			OVNBFDMinRxMs:     150,
			OVNBFDMinTxMs:     150,
			FRRBFDMinRxMs:     150,
			FRRBFDMinTxMs:     150,
			FRRBFDMultiplier:  3,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults are valid", func(*Config) {}, false},
		{"check enabled with zero max detect", func(c *Config) { c.BFDCheckMaxDetect = 0 }, true},
		{"check enabled with negative max detect", func(c *Config) { c.BFDCheckMaxDetect = -time.Second }, true},
		{"check disabled tolerates zero max detect", func(c *Config) {
			c.BFDCheckEnabled = false
			c.BFDCheckMaxDetect = 0
		}, false},
		{"ovn manage with zero min_rx", func(c *Config) { c.OVNBFDManage = true; c.OVNBFDMinRxMs = 0 }, true},
		{"ovn manage with oversized min_tx", func(c *Config) { c.OVNBFDManage = true; c.OVNBFDMinTxMs = 60001 }, true},
		{"ovn manage disabled tolerates zero min_rx", func(c *Config) { c.OVNBFDMinRxMs = 0 }, false},
		{"frr manage with too-small min_rx", func(c *Config) { c.FRRBFDManage = true; c.FRRBFDMinRxMs = 9 }, true},
		{"frr manage with too-small min_tx", func(c *Config) { c.FRRBFDManage = true; c.FRRBFDMinTxMs = 0 }, true},
		{"frr manage with multiplier 1", func(c *Config) { c.FRRBFDManage = true; c.FRRBFDMultiplier = 1 }, true},
		{"frr manage with multiplier 256", func(c *Config) { c.FRRBFDManage = true; c.FRRBFDMultiplier = 256 }, true},
		{"frr manage disabled tolerates multiplier 1", func(c *Config) { c.FRRBFDMultiplier = 1 }, false},
		{"both manage enabled with valid values", func(c *Config) { c.OVNBFDManage = true; c.FRRBFDManage = true }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := base()
			tt.mutate(&cfg)
			err := validateConfig(&cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfig() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
