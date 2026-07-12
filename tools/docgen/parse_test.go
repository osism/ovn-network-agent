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
	"fmt"
	"os"
	"time"
)

type PortForwardRule struct {
	Proto string ` + "`yaml:\"proto\"`" + ` // "tcp" or "udp"
}

type Config struct {
	BridgeDev         string
	RouteTableID      int
	ReconcileInterval time.Duration
	DryRun            bool
	OVNSBRemote       string
}

type configFile struct {
	BridgeDev         string ` + "`yaml:\"bridge_dev\"`" + `
	RouteTableID      *int   ` + "`yaml:\"route_table_id\"`" + `
	ReconcileInterval string ` + "`yaml:\"reconcile_interval\"`" + `
	DryRun            *bool  ` + "`yaml:\"dry_run\"`" + `
	OVNSBRemote       string ` + "`yaml:\"ovn_sb_remote\"`" + `
}

func loadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("ovn-network-agent", flag.ContinueOnError)
	var (
		configPath = fs.String("config", os.Getenv("OVN_NETWORK_CONFIG"), "Path to YAML config file")
		fBridge    = fs.String("bridge-dev", "", "Provider bridge device")
		fTableID   = fs.Int("route-table-id", 0, "Routing table ID (1-252); 0 = main")
		fInterval  = fs.String("reconcile-interval", "", "Reconcile interval (e.g. 60s)")
		fDryRun    = fs.Bool("dry-run", false, "Dry-run mode")
		fOVNSB     = fs.String("ovn-sb-remote", "", "OVN SB remote")
	)
	_ = configPath
	cfg := Config{
		BridgeDev:         "br-ex",
		ReconcileInterval: 60 * time.Second,
	}
	var flagErr error
	fs.Visit(func(f *flag.Flag) {
		if flagErr != nil {
			return
		}
		switch f.Name {
		case "bridge-dev":
			cfg.BridgeDev = *fBridge
		case "route-table-id":
			cfg.RouteTableID = *fTableID
		case "reconcile-interval":
			d, err := time.ParseDuration(*fInterval)
			if err != nil {
				flagErr = fmt.Errorf("invalid -reconcile-interval %q: %w", *fInterval, err)
				return
			}
			cfg.ReconcileInterval = d
		case "dry-run":
			cfg.DryRun = *fDryRun
		case "ovn-sb-remote":
			cfg.OVNSBRemote = *fOVNSB
		}
	})
	return cfg, nil
}

func applyFileConfig(cfg *Config, fc *configFile) error {
	if fc.BridgeDev != "" {
		cfg.BridgeDev = fc.BridgeDev
	}
	if fc.RouteTableID != nil {
		cfg.RouteTableID = *fc.RouteTableID
	}
	if fc.ReconcileInterval != "" {
		d, err := time.ParseDuration(fc.ReconcileInterval)
		if err != nil {
			return fmt.Errorf("invalid reconcile_interval %q: %w", fc.ReconcileInterval, err)
		}
		cfg.ReconcileInterval = d
	}
	if fc.DryRun != nil {
		cfg.DryRun = *fc.DryRun
	}
	if fc.OVNSBRemote != "" {
		cfg.OVNSBRemote = fc.OVNSBRemote
	}
	return nil
}

func applyEnvConfig(cfg *Config) error {
	if v := os.Getenv("OVN_NETWORK_BRIDGE_DEV"); v != "" {
		cfg.BridgeDev = v
	}
	if v := os.Getenv("OVN_NETWORK_OVN_SB_REMOTE"); v != "" {
		cfg.OVNSBRemote = v
	}
	if v := os.Getenv("OVN_NETWORK_RECONCILE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid OVN_NETWORK_RECONCILE_INTERVAL %q: %w", v, err)
		}
		cfg.ReconcileInterval = d
	}
	return nil
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

	wantFlags := []string{
		"config", "bridge-dev", "route-table-id",
		"reconcile-interval", "dry-run", "ovn-sb-remote",
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
