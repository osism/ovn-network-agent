package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFixture writes a single-file Go source tree to a fresh
// temporary directory and returns its path. Tests use it to feed the
// AST parser without standing up the whole repo.
func writeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

const configFixture = `package main

import (
	"flag"
	"os"
	"time"
)

type PortForwardRule struct {
	Proto string ` + "`yaml:\"proto\"`" + ` // "tcp" or "udp"
}

type PortForwardVIP struct {
	VIP string ` + "`yaml:\"vip\"`" + ` // anycast VIP address
}

type Config struct {
	BridgeDev         string
	RouteTableID      int
	ReconcileInterval time.Duration
	DryRun            bool
	OVNSBRemote       string
	NetworkCIDRs      []string
	PortForwards      []PortForwardVIP // VIP forwarding rules from config
}

type configOption struct {
	Flag  string
	Key   string
	Usage string
}

func configOptions() []configOption {
	return []configOption{
		stringOpt("ovn-sb-remote", "", "OVN SB remote",
			func(c *Config) *string { return &c.OVNSBRemote }),
		stringOpt("bridge-dev", "br-ex", "Provider bridge device",
			func(c *Config) *string { return &c.BridgeDev }),
		intOpt("route-table-id", 0, "Routing table ID (1-252); 0 = main",
			func(c *Config) *int { return &c.RouteTableID }),
		durationOpt("reconcile-interval", 60*time.Second, "Reconcile interval (e.g. 60s)",
			func(c *Config) *time.Duration { return &c.ReconcileInterval }),
		boolOpt("dry-run", false, "Dry-run mode",
			func(c *Config) *bool { return &c.DryRun }),
		stringSliceOpt("network-cidr", "Filter FIPs by CIDRs",
			func(c *Config) *[]string { return &c.NetworkCIDRs }),
		portForwardsOpt("port_forwards", "See sample config for usage.",
			func(c *Config) *[]PortForwardVIP { return &c.PortForwards }),
	}
}

func loadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("ovn-network-agent", flag.ContinueOnError)
	configPath := fs.String("config", os.Getenv("OVN_NETWORK_CONFIG"), "Path to YAML config file")
	_ = configPath
	for _, o := range configOptions() {
		if o.register != nil {
			o.register(fs)
		}
	}
	return Config{}, nil
}
`

const metricsFixture = `package main

import "github.com/prometheus/client_golang/prometheus"

const metricsNamespace = "ovn_network_agent"

type metricsRegistry struct {
	reconcileTotal    *prometheus.CounterVec
	reconcileDuration prometheus.Histogram
	desiredIPs        prometheus.Gauge
}

func newMetricsRegistry() *metricsRegistry {
	m := &metricsRegistry{
		reconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "reconcile_total",
			Help:      "Total reconcile cycles, labelled by trigger source.",
		}, []string{"trigger"}),

		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "reconcile_duration_seconds",
			Help:      "Duration of a single reconcile cycle in seconds.",
		}),

		desiredIPs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "desired_ips",
			Help:      "Unique IPs the agent wants routes for.",
		}),
	}

	m.reconcileTotal.WithLabelValues("event").Add(0)
	m.reconcileTotal.WithLabelValues("periodic").Add(0)
	m.reconcileTotal.WithLabelValues("startup").Add(0)

	return m
}
`

func TestParseSource_Flags(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	// The CLI-only action flags come first (they are declared directly on
	// the FlagSet), then the option registry in table order.
	wantFlags := []string{
		"config",
		"ovn-sb-remote", "bridge-dev", "route-table-id",
		"reconcile-interval", "dry-run", "network-cidr",
	}
	if len(info.Flags) != len(wantFlags) {
		t.Fatalf("got %d flags, want %d (%v)", len(info.Flags), len(wantFlags), flagNames(info.Flags))
	}
	for i, want := range wantFlags {
		if info.Flags[i].Name != want {
			t.Errorf("Flags[%d].Name = %q, want %q", i, info.Flags[i].Name, want)
		}
	}
}

// TestParseSource_YAMLOnlyOption pins the registry's flag-less rows: they must
// surface as YAML-only keys (with their Config field resolved so the reference
// table can show the Go type), not as phantom CLI flags.
func TestParseSource_YAMLOnlyOption(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	if len(info.YAMLOnly) != 1 {
		t.Fatalf("got %d YAML-only options, want 1: %+v", len(info.YAMLOnly), info.YAMLOnly)
	}
	got := info.YAMLOnly[0]
	if got.Key != "port_forwards" || got.ConfigField != "PortForwards" {
		t.Errorf("YAMLOnly[0] = %+v, want key=port_forwards field=PortForwards", got)
	}
	// It must not also appear as a CLI flag.
	for _, fl := range info.Flags {
		if fl.Name == "port_forwards" {
			t.Error("the YAML-only option leaked into the flag list")
		}
	}
	// And its YAML key must still be resolvable by Config field.
	if info.YAMLByField["PortForwards"] != "port_forwards" {
		t.Errorf("YAMLByField[PortForwards] = %q, want port_forwards", info.YAMLByField["PortForwards"])
	}
}

// TestParseSource_DerivedEnvAndYAMLNames pins the derivation the registry
// relies on: an option's env var and YAML key follow mechanically from its flag
// name, so no row can declare them inconsistently.
func TestParseSource_DerivedEnvAndYAMLNames(t *testing.T) {
	cases := []struct {
		flag, env, yaml string
	}{
		{"ovn-sb-remote", "OVN_NETWORK_OVN_SB_REMOTE", "ovn_sb_remote"},
		{"reconcile-interval", "OVN_NETWORK_RECONCILE_INTERVAL", "reconcile_interval"},
		{"port-forward-l3mdev-accept", "OVN_NETWORK_PORT_FORWARD_L3MDEV_ACCEPT", "port_forward_l3mdev_accept"},
	}
	for _, tc := range cases {
		if got := envVarName(tc.flag); got != tc.env {
			t.Errorf("envVarName(%q) = %q, want %q", tc.flag, got, tc.env)
		}
		if got := yamlKeyName(tc.flag); got != tc.yaml {
			t.Errorf("yamlKeyName(%q) = %q, want %q", tc.flag, got, tc.yaml)
		}
	}
}

func TestParseSource_ImplicitEnvFromGetenvDefault(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	var cfgFlag *flagInfo
	for i := range info.Flags {
		if info.Flags[i].Name == "config" {
			cfgFlag = &info.Flags[i]
			break
		}
	}
	if cfgFlag == nil {
		t.Fatalf("--config flag not parsed")
	}
	if cfgFlag.ImplicitEnv != "OVN_NETWORK_CONFIG" {
		t.Errorf("ImplicitEnv = %q, want OVN_NETWORK_CONFIG", cfgFlag.ImplicitEnv)
	}
	if cfgFlag.Default != "" {
		t.Errorf("Default = %q, want empty for os.Getenv-backed default", cfgFlag.Default)
	}
}

func TestParseSource_DefaultsFromCompositeLiteral(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	if got := info.DefaultByField["BridgeDev"]; got != `"br-ex"` {
		t.Errorf("DefaultByField[BridgeDev] = %q, want \"br-ex\"", got)
	}
	if got := info.DefaultByField["ReconcileInterval"]; got != "60 * time.Second" {
		t.Errorf("DefaultByField[ReconcileInterval] = %q, want 60 * time.Second", got)
	}
}

func TestParseSource_YAMLMapping(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	cases := map[string]string{
		"BridgeDev":         "bridge_dev",
		"RouteTableID":      "route_table_id",
		"ReconcileInterval": "reconcile_interval", // wrapped in time.ParseDuration
		"DryRun":            "dry_run",
		"OVNSBRemote":       "ovn_sb_remote",
	}
	for field, want := range cases {
		if got := info.YAMLByField[field]; got != want {
			t.Errorf("YAMLByField[%s] = %q, want %q", field, got, want)
		}
	}
}

func TestParseSource_EnvMapping(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	cases := map[string]string{
		"BridgeDev":         "OVN_NETWORK_BRIDGE_DEV",
		"OVNSBRemote":       "OVN_NETWORK_OVN_SB_REMOTE",
		"ReconcileInterval": "OVN_NETWORK_RECONCILE_INTERVAL",
	}
	for field, want := range cases {
		if got := info.EnvByField[field]; got != want {
			t.Errorf("EnvByField[%s] = %q, want %q", field, got, want)
		}
	}
}

func TestParseSource_FlagToConfigField(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	cases := map[string]string{
		"bridge-dev":         "BridgeDev",
		"route-table-id":     "RouteTableID",
		"reconcile-interval": "ReconcileInterval",
		"dry-run":            "DryRun",
		"ovn-sb-remote":      "OVNSBRemote",
	}
	for _, fl := range info.Flags {
		want, ok := cases[fl.Name]
		if !ok {
			continue
		}
		if fl.ConfigField != want {
			t.Errorf("flag %q ConfigField = %q, want %q", fl.Name, fl.ConfigField, want)
		}
	}
}

func TestParseSource_Metrics(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	if info.Namespace != "ovn_network_agent" {
		t.Errorf("Namespace = %q, want ovn_network_agent", info.Namespace)
	}
	if len(info.Metrics) != 3 {
		t.Fatalf("got %d metrics, want 3", len(info.Metrics))
	}
	want := []metricInfo{
		{Name: "reconcile_total", FullName: "ovn_network_agent_reconcile_total", Kind: "counter", IsVec: true, Labels: []string{"trigger"}, Help: "Total reconcile cycles, labelled by trigger source."},
		{Name: "reconcile_duration_seconds", FullName: "ovn_network_agent_reconcile_duration_seconds", Kind: "histogram", Help: "Duration of a single reconcile cycle in seconds."},
		{Name: "desired_ips", FullName: "ovn_network_agent_desired_ips", Kind: "gauge", Help: "Unique IPs the agent wants routes for."},
	}
	for i, w := range want {
		got := info.Metrics[i]
		if got.Name != w.Name || got.FullName != w.FullName || got.Kind != w.Kind || got.IsVec != w.IsVec || got.Help != w.Help {
			t.Errorf("metric[%d] = %+v, want %+v", i, got, w)
		}
		if len(got.Labels) != len(w.Labels) {
			t.Errorf("metric[%d] labels = %v, want %v", i, got.Labels, w.Labels)
			continue
		}
		for j, lbl := range w.Labels {
			if got.Labels[j] != lbl {
				t.Errorf("metric[%d].Labels[%d] = %q, want %q", i, j, got.Labels[j], lbl)
			}
		}
	}
}

func TestParseSource_MetricLabelValues(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	var reconcile *metricInfo
	for i := range info.Metrics {
		if info.Metrics[i].Name == "reconcile_total" {
			reconcile = &info.Metrics[i]
			break
		}
	}
	if reconcile == nil {
		t.Fatalf("reconcile_total metric not parsed")
	}
	got := reconcile.LabelValues["trigger"]
	want := []string{"event", "periodic", "startup"}
	if len(got) != len(want) {
		t.Fatalf("LabelValues[trigger] = %v, want %v", got, want)
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("LabelValues[trigger][%d] = %q, want %q", i, got[i], v)
		}
	}
}

func TestParseSource_StructTags(t *testing.T) {
	dir := writeFixture(t, map[string]string{
		"config.go":  configFixture,
		"metrics.go": metricsFixture,
	})

	info, err := parseSource(dir)
	if err != nil {
		t.Fatalf("parseSource: %v", err)
	}

	st := info.Structs["PortForwardRule"]
	if st == nil {
		t.Fatalf("PortForwardRule not parsed")
	}
	if len(st.Fields) != 1 || st.Fields[0].YAMLTag != "proto" {
		t.Fatalf("unexpected PortForwardRule fields: %+v", st.Fields)
	}
	if !strings.Contains(st.Fields[0].Comment, "tcp") {
		t.Errorf("PortForwardRule.Proto comment = %q, want it to mention tcp", st.Fields[0].Comment)
	}
}

func flagNames(flags []flagInfo) []string {
	out := make([]string, len(flags))
	for i, fl := range flags {
		out[i] = fl.Name
	}
	return out
}
