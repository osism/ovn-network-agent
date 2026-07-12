package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingEnvVars(t *testing.T) {
	// BRIDGE_DEV is present but commented; OVN_SB_REMOTE is present and
	// uncommented; METRICS_LISTEN is absent entirely.
	content := "#OVN_NETWORK_BRIDGE_DEV=br-ex\nOVN_NETWORK_OVN_SB_REMOTE=tcp:10.0.0.1:6642\n"
	vars := []string{
		"OVN_NETWORK_BRIDGE_DEV",
		"OVN_NETWORK_OVN_SB_REMOTE",
		"OVN_NETWORK_METRICS_LISTEN",
	}
	got := missingEnvVars(content, vars)
	if len(got) != 1 || got[0] != "OVN_NETWORK_METRICS_LISTEN" {
		t.Errorf("missingEnvVars = %v, want [OVN_NETWORK_METRICS_LISTEN]", got)
	}
}

func TestMissingEnvVarsPrefixIsNotAMatch(t *testing.T) {
	// A shorter var name must not be satisfied by a longer one that shares
	// its prefix — the trailing "=" anchor guards against that.
	content := "#OVN_NETWORK_DRAIN_TIMEOUT=60s\n"
	got := missingEnvVars(content, []string{"OVN_NETWORK_DRAIN"})
	if len(got) != 1 || got[0] != "OVN_NETWORK_DRAIN" {
		t.Errorf("missingEnvVars = %v, want [OVN_NETWORK_DRAIN] (prefix must not match)", got)
	}
}

func TestMissingYAMLKeys(t *testing.T) {
	// bridge_dev commented, network_cidr uncommented, vip/manage_vip as
	// commented list items, dest_addrs as a deeply-indented commented key.
	// dest_addr is absent and must NOT be satisfied by dest_addrs.
	content := `
# bridge_dev: "br-ex"
network_cidr:
  - "10.0.0.0/24"
#   - vip: "198.51.100.10"
#     manage_vip: true
#         dest_addrs:
`
	keys := []string{"bridge_dev", "network_cidr", "vip", "manage_vip", "dest_addrs", "dest_addr"}
	got := missingYAMLKeys(content, keys)
	if len(got) != 1 || got[0] != "dest_addr" {
		t.Errorf("missingYAMLKeys = %v, want [dest_addr] (dest_addrs must not satisfy dest_addr)", got)
	}
}

func TestRequiredEnvVarsIncludesImplicitEnv(t *testing.T) {
	info := &sourceInfo{
		EnvByField: map[string]string{
			"BridgeDev":   "OVN_NETWORK_BRIDGE_DEV",
			"OVNSBRemote": "OVN_NETWORK_OVN_SB_REMOTE",
		},
		Flags: []flagInfo{
			{Name: "config", ImplicitEnv: "OVN_NETWORK_CONFIG"},
		},
	}
	got := requiredEnvVars(info)
	want := []string{
		"OVN_NETWORK_BRIDGE_DEV",
		"OVN_NETWORK_CONFIG",
		"OVN_NETWORK_OVN_SB_REMOTE",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requiredEnvVars = %v, want %v", got, want)
	}
}

func TestRequiredYAMLKeysFollowsNestedStructFields(t *testing.T) {
	// configFile reaches PortForwardVIP through a struct-typed field,
	// which reaches PortForwardRule, which reaches a further nested
	// struct. requiredYAMLKeys must discover every key by following those
	// fields rather than a hardcoded list of struct names — otherwise a
	// newly-added nested struct (here PortForwardHealthCheck, reachable
	// only via PortForwardRule) is silently dropped from the required set.
	info := &sourceInfo{
		Structs: map[string]*structInfo{
			"configFile": {Name: "configFile", Fields: []structField{
				{Name: "BridgeDev", Type: "string", YAMLTag: "bridge_dev"},
				{Name: "PortForwards", Type: "[]PortForwardVIP", YAMLTag: "port_forwards"},
			}},
			"PortForwardVIP": {Name: "PortForwardVIP", Fields: []structField{
				{Name: "VIP", Type: "string", YAMLTag: "vip"},
				{Name: "ManageVIP", Type: "bool", YAMLTag: "manage_vip"},
				{Name: "Rules", Type: "[]PortForwardRule", YAMLTag: "rules"},
			}},
			"PortForwardRule": {Name: "PortForwardRule", Fields: []structField{
				{Name: "Proto", Type: "string", YAMLTag: "proto"},
				{Name: "DestAddr", Type: "string", YAMLTag: "dest_addr"},
				{Name: "HealthCheck", Type: "*PortForwardHealthCheck", YAMLTag: "health_check"},
			}},
			"PortForwardHealthCheck": {Name: "PortForwardHealthCheck", Fields: []structField{
				{Name: "Path", Type: "string", YAMLTag: "path"},
			}},
		},
	}
	got := requiredYAMLKeys(info)
	for _, want := range []string{"bridge_dev", "port_forwards", "vip", "manage_vip", "rules", "proto", "dest_addr", "health_check", "path"} {
		if !containsString(got, want) {
			t.Errorf("requiredYAMLKeys = %v, missing %q", got, want)
		}
	}
}

func TestCheckSamples(t *testing.T) {
	info := &sourceInfo{
		EnvByField: map[string]string{"BridgeDev": "OVN_NETWORK_BRIDGE_DEV"},
		Flags:      []flagInfo{{Name: "config", ImplicitEnv: "OVN_NETWORK_CONFIG"}},
		Structs: map[string]*structInfo{
			"configFile": {Name: "configFile", Fields: []structField{
				{Name: "BridgeDev", YAMLTag: "bridge_dev"},
			}},
		},
	}

	writeSamples := func(t *testing.T, def, sample string) string {
		t.Helper()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "ovn-network-agent.default"), []byte(def), 0o644); err != nil {
			t.Fatalf("write default: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "ovn-network-agent.yaml.sample"), []byte(sample), 0o644); err != nil {
			t.Fatalf("write sample: %v", err)
		}
		return root
	}

	t.Run("complete files pass", func(t *testing.T) {
		root := writeSamples(t,
			"#OVN_NETWORK_CONFIG=/etc/x\n#OVN_NETWORK_BRIDGE_DEV=br-ex\n",
			"# bridge_dev: \"br-ex\"\n",
		)
		if err := checkSamples(root, info); err != nil {
			t.Errorf("checkSamples() = %v, want nil", err)
		}
	})

	t.Run("missing env var fails naming file and var", func(t *testing.T) {
		root := writeSamples(t,
			"#OVN_NETWORK_CONFIG=/etc/x\n", // BRIDGE_DEV omitted
			"# bridge_dev: \"br-ex\"\n",
		)
		err := checkSamples(root, info)
		if err == nil {
			t.Fatal("checkSamples() = nil, want error for missing env var")
		}
		if !strings.Contains(err.Error(), "ovn-network-agent.default") || !strings.Contains(err.Error(), "OVN_NETWORK_BRIDGE_DEV") {
			t.Errorf("error %q does not name the file and the missing var", err)
		}
	})

	t.Run("missing YAML key fails naming file and key", func(t *testing.T) {
		root := writeSamples(t,
			"#OVN_NETWORK_CONFIG=/etc/x\n#OVN_NETWORK_BRIDGE_DEV=br-ex\n",
			"# nothing useful here\n", // bridge_dev omitted
		)
		err := checkSamples(root, info)
		if err == nil {
			t.Fatal("checkSamples() = nil, want error for missing YAML key")
		}
		if !strings.Contains(err.Error(), "ovn-network-agent.yaml.sample") || !strings.Contains(err.Error(), "bridge_dev") {
			t.Errorf("error %q does not name the file and the missing key", err)
		}
	})
}
