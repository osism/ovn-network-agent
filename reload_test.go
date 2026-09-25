package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// reloadConfigFile writes content to a temp config file and returns its path.
func reloadConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// loadedConfig runs the real loader in full mode with the extra flags, the way
// main builds the running configuration and a reload builds the next one.
func loadedConfig(t *testing.T, extra ...string) Config {
	t.Helper()
	cfg, err := loadConfig(fullModeArgs(extra...))
	if err != nil {
		t.Fatalf("loadConfig(%v): %v", extra, err)
	}
	return cfg
}

// pfYAML renders a port_forwards block with one tcp/443 rule per VIP, every
// VIP managed.
func pfYAML(vips ...string) string {
	var b strings.Builder
	b.WriteString("port_forwards:\n")
	for _, v := range vips {
		b.WriteString("  - vip: \"" + v + "\"\n")
		b.WriteString("    manage_vip: true\n")
		b.WriteString("    rules:\n")
		b.WriteString("      - proto: tcp\n")
		b.WriteString("        port: 443\n")
		b.WriteString("        dest_addr: \"10.0.0.100\"\n")
	}
	return b.String()
}

// reloadAgent builds an agent running cfg whose reloads return next. Its
// RouteManager records vtysh calls instead of running them.
func reloadAgent(t *testing.T, running Config, next func() (Config, error)) (*Agent, *vtyshRecorder) {
	t.Helper()
	rec := newVtyshRecorder()
	rm := NewRouteManager(running)
	rm.execVtyshHook = rec.hook()
	return &Agent{
		cfg:            running,
		routing:        rm,
		reconcileCh:    make(chan struct{}, 1),
		reloadCh:       make(chan struct{}, 1),
		reloadConfig:   next,
		missingChassis: make(map[string]time.Time),
	}, rec
}

// returning makes a reload loader that always returns cfg.
func returning(cfg Config) func() (Config, error) {
	return func() (Config, error) { return cfg, nil }
}

// keepLogLevel restores the process-wide log level after a test that reloads
// log_level.
func keepLogLevel(t *testing.T) {
	t.Helper()
	prev := logLevel.Level()
	t.Cleanup(func() { logLevel.Set(prev) })
}

// copyPEM overwrites dst with src's contents, keeping dst's mode, as an
// operator rotating certificate files in place would.
func copyPEM(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

func restartKeys(p reloadPlan) []string {
	keys := []string{}
	for _, c := range p.restartRequired {
		keys = append(keys, c.Key)
	}
	return keys
}

func TestPlanReloadNoChange(t *testing.T) {
	cfg := loadedConfig(t)
	p, err := planReload(cfg, cfg)
	if err != nil {
		t.Fatalf("planReload() error = %v", err)
	}
	if len(p.applied) != 0 || len(p.restartRequired) != 0 {
		t.Errorf("applied = %v, restartRequired = %v, want both empty", p.applied, p.restartRequired)
	}
	if !reflect.DeepEqual(p.merged, cfg) {
		t.Errorf("merged = %+v, want the running config %+v", p.merged, cfg)
	}
}

func TestPlanReloadAppliesReloadableKeys(t *testing.T) {
	running := loadedConfig(t)
	next := loadedConfig(t,
		"--log-level", "debug",
		"--stale-chassis-grace-period", "1m",
		"--reconcile-interval", "30s",
	)

	p, err := planReload(running, next)
	if err != nil {
		t.Fatalf("planReload() error = %v", err)
	}
	want := []string{"reconcile_interval", "log_level", "stale_chassis_grace_period"}
	if !reflect.DeepEqual(p.applied, want) {
		t.Errorf("applied = %v, want %v (registry order)", p.applied, want)
	}
	if len(p.restartRequired) != 0 {
		t.Errorf("restartRequired = %v, want none", p.restartRequired)
	}
	if p.merged.LogLevel != "debug" || p.merged.StaleChassisGracePeriod != time.Minute || p.merged.ReconcileInterval != 30*time.Second {
		t.Errorf("merged = log %q grace %v interval %v, want the new values",
			p.merged.LogLevel, p.merged.StaleChassisGracePeriod, p.merged.ReconcileInterval)
	}
}

func TestPlanReloadKeepsRestartOnlyKeys(t *testing.T) {
	running := loadedConfig(t)
	next := loadedConfig(t, "--bridge-dev", "br-other", "--log-level", "debug")

	p, err := planReload(running, next)
	if err != nil {
		t.Fatalf("planReload() error = %v", err)
	}
	if want := []string{"log_level"}; !reflect.DeepEqual(p.applied, want) {
		t.Errorf("applied = %v, want %v", p.applied, want)
	}
	if want := []restartChange{{"bridge_dev", reasonStartupOnly}}; !reflect.DeepEqual(p.restartRequired, want) {
		t.Errorf("restartRequired = %v, want %v", p.restartRequired, want)
	}
	if p.merged.BridgeDev != running.BridgeDev {
		t.Errorf("merged.BridgeDev = %q, want the running %q", p.merged.BridgeDev, running.BridgeDev)
	}
}

func TestPlanReloadPortForwardToggleNeedsRestart(t *testing.T) {
	without := loadedConfig(t)
	with := loadedConfig(t, "--config", reloadConfigFile(t, pfYAML("198.51.100.30")))

	for _, tc := range []struct {
		name          string
		running, next Config
	}{
		{"non-empty to empty", with, without},
		{"empty to non-empty", without, with},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := planReload(tc.running, tc.next)
			if err != nil {
				t.Fatalf("planReload() error = %v", err)
			}
			want := []restartChange{{"port_forwards", reasonPortForwardToggle}}
			if !reflect.DeepEqual(p.restartRequired, want) {
				t.Errorf("restartRequired = %v, want %v", p.restartRequired, want)
			}
			if !reflect.DeepEqual(p.merged.PortForwards, tc.running.PortForwards) {
				t.Errorf("merged.PortForwards = %v, want the running list %v", p.merged.PortForwards, tc.running.PortForwards)
			}
		})
	}
}

func TestPlanReloadClientCertToggle(t *testing.T) {
	certPath, keyPath := writeTestTLSFiles(t)

	t.Run("adding a client certificate needs a restart", func(t *testing.T) {
		running := loadedConfig(t)
		next := loadedConfig(t, "--ovn-ssl-cert", certPath, "--ovn-ssl-key", keyPath)
		p, err := planReload(running, next)
		if err != nil {
			t.Fatalf("planReload() error = %v", err)
		}
		want := []restartChange{
			{"ovn_ssl_cert", reasonClientCertToggle},
			{"ovn_ssl_key", reasonClientCertToggle},
		}
		if !reflect.DeepEqual(p.restartRequired, want) {
			t.Errorf("restartRequired = %v, want %v", p.restartRequired, want)
		}
		if p.merged.OVNClientCert != nil {
			t.Error("merged.OVNClientCert set, want the running nil")
		}
	})

	t.Run("no client certificate on either side", func(t *testing.T) {
		running := loadedConfig(t)
		next := loadedConfig(t, "--log-level", "debug")
		p, err := planReload(running, next)
		if err != nil {
			t.Fatalf("planReload() error = %v", err)
		}
		for _, k := range append(append([]string{}, p.applied...), restartKeys(p)...) {
			if strings.HasPrefix(k, "ovn_ssl_") {
				t.Errorf("plan lists %s, want no TLS entry", k)
			}
		}
	})
}

func TestPlanReloadClientCertRotatedInPlace(t *testing.T) {
	certPath, keyPath := writeTestTLSFiles(t)
	args := []string{"--ovn-ssl-cert", certPath, "--ovn-ssl-key", keyPath}
	running := loadedConfig(t, args...)

	newCert, newKey := writeTestTLSFiles(t)
	copyPEM(t, newCert, certPath)
	copyPEM(t, newKey, keyPath)
	next := loadedConfig(t, args...)

	p, err := planReload(running, next)
	if err != nil {
		t.Fatalf("planReload() error = %v", err)
	}
	if want := []string{"ovn_ssl_cert"}; !reflect.DeepEqual(p.applied, want) {
		t.Errorf("applied = %v, want %v", p.applied, want)
	}
	if len(p.restartRequired) != 0 {
		t.Errorf("restartRequired = %v, want none", p.restartRequired)
	}
}

func TestPlanReloadCAContentChangeNeedsRestart(t *testing.T) {
	caPath, _ := writeTestTLSFiles(t)
	running := loadedConfig(t, "--ovn-ssl-ca", caPath)

	otherCA, _ := writeTestTLSFiles(t)
	copyPEM(t, otherCA, caPath)
	next := loadedConfig(t, "--ovn-ssl-ca", caPath)

	p, err := planReload(running, next)
	if err != nil {
		t.Fatalf("planReload() error = %v", err)
	}
	want := []restartChange{{"ovn_ssl_ca", reasonCAContent}}
	if !reflect.DeepEqual(p.restartRequired, want) {
		t.Errorf("restartRequired = %v, want %v", p.restartRequired, want)
	}
	if p.merged.OVNTLS != running.OVNTLS {
		t.Error("merged.OVNTLS was replaced, want the running tls.Config")
	}
}

// TestPlanReloadRejectsInvalidMerge covers a file that is valid on its own but
// not once merged: it adds OVN remotes (restart-only, so kept empty) and a
// router_masquerade VIP (applied) to a port-forward-only agent.
func TestPlanReloadRejectsInvalidMerge(t *testing.T) {
	running, err := loadConfig([]string{"--config", reloadConfigFile(t, pfYAML("198.51.100.30"))})
	if err != nil {
		t.Fatalf("load pf-only running config: %v", err)
	}
	if !running.PortForwardOnly {
		t.Fatal("running config is not port-forward-only")
	}
	nextYAML := pfYAML("198.51.100.30") + "    router_masquerade: true\n"
	next := loadedConfig(t, "--config", reloadConfigFile(t, nextYAML))

	_, err = planReload(running, next)
	if err == nil {
		t.Fatal("planReload() error = nil, want the merged config rejected")
	}
	for _, want := range []string{"validate reloaded configuration:", "router_masquerade requires an OVN connection"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestReloadableKeysExist(t *testing.T) {
	keys := make(map[string]bool)
	for _, o := range configOptions() {
		keys[o.Key] = true
	}
	for k := range reloadableKeys {
		if !keys[k] {
			t.Errorf("reloadableKeys lists %q, which is no option in configOptions()", k)
		}
	}
}

func TestConfigOptionsHaveValueAccessors(t *testing.T) {
	src := loadedConfig(t,
		"--config", reloadConfigFile(t, pfYAML("198.51.100.30")),
		"--network-cidr", "10.0.0.0/24",
		"--log-level", "debug",
	)
	for _, o := range configOptions() {
		if o.value == nil || o.copyValue == nil {
			t.Errorf("option %s has no value/copyValue accessor", o.Key)
			continue
		}
		var dst Config
		o.copyValue(&dst, &src)
		if !reflect.DeepEqual(o.value(&dst), o.value(&src)) {
			t.Errorf("option %s: copyValue did not copy the value", o.Key)
		}
	}
}

func TestWithdrawnManagedVIPs(t *testing.T) {
	managed := func(vip string) PortForwardVIP { return PortForwardVIP{VIP: vip, ManageVIP: true} }
	unmanaged := func(vip string) PortForwardVIP { return PortForwardVIP{VIP: vip} }

	tests := []struct {
		name          string
		running, next []PortForwardVIP
		want          []string
	}{
		{"managed VIP dropped",
			[]PortForwardVIP{managed("198.51.100.30"), managed("198.51.100.31")},
			[]PortForwardVIP{managed("198.51.100.30")},
			[]string{"198.51.100.31"}},
		{"manage_vip flipped off",
			[]PortForwardVIP{managed("198.51.100.30")},
			[]PortForwardVIP{unmanaged("198.51.100.30")},
			[]string{"198.51.100.30"}},
		{"unmanaged VIP dropped",
			[]PortForwardVIP{managed("198.51.100.30"), unmanaged("198.51.100.31")},
			[]PortForwardVIP{managed("198.51.100.30")},
			nil},
		{"nil lists", nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withdrawnManagedVIPs(tt.running, tt.next); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("withdrawnManagedVIPs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequestReloadCoalesces(t *testing.T) {
	a, err := NewAgent(loadedConfig(t), nil)
	if err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
	for range 3 {
		a.RequestReload() // must never block without a running loop
	}
	if got := len(a.reloadCh); got != 1 {
		t.Errorf("pending reload requests = %d, want 1", got)
	}
}

func TestHandleReloadAppliesReloadableChange(t *testing.T) {
	m := withTestMetrics(t)
	buf := captureSlog(t)
	running := loadedConfig(t)
	next := loadedConfig(t, "--stale-chassis-grace-period", "1m")
	a, _ := reloadAgent(t, running, returning(next))

	if a.handleReload() {
		t.Error("handleReload() = true, want false (reconcile_interval unchanged)")
	}
	if a.cfg.StaleChassisGracePeriod != time.Minute || a.routing.cfg.StaleChassisGracePeriod != time.Minute {
		t.Errorf("grace period agent %v routing %v, want 1m in both",
			a.cfg.StaleChassisGracePeriod, a.routing.cfg.StaleChassisGracePeriod)
	}
	if got := counterValue(t, m, "ovn_network_agent_config_reload_total", "outcome", "success"); got != 1 {
		t.Errorf("config_reload_total{success} = %v, want 1", got)
	}
	if got := len(a.reconcileCh); got != 1 {
		t.Errorf("queued reconciles = %d, want 1", got)
	}
	for _, want := range []string{`msg="configuration reloaded"`, "applied=[stale_chassis_grace_period]"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q does not contain %q", buf.String(), want)
		}
	}
}

func TestHandleReloadReportsIntervalChange(t *testing.T) {
	withTestMetrics(t)
	captureSlog(t)
	next := loadedConfig(t, "--reconcile-interval", "1s")
	a, _ := reloadAgent(t, loadedConfig(t), returning(next))

	if !a.handleReload() {
		t.Error("handleReload() = false, want true when reconcile_interval changed")
	}
	if a.cfg.ReconcileInterval != time.Second {
		t.Errorf("ReconcileInterval = %v, want 1s", a.cfg.ReconcileInterval)
	}
}

func TestHandleReloadLoaderErrorKeepsRunningConfig(t *testing.T) {
	m := withTestMetrics(t)
	buf := captureSlog(t)
	running := loadedConfig(t)
	a, _ := reloadAgent(t, running, func() (Config, error) { return Config{}, errors.New("boom") })

	a.handleReload()

	if !reflect.DeepEqual(a.cfg, running) {
		t.Errorf("config changed on a failed reload: %+v", a.cfg)
	}
	if got := counterValue(t, m, "ovn_network_agent_config_reload_total", "outcome", "error"); got != 1 {
		t.Errorf("config_reload_total{error} = %v, want 1", got)
	}
	if got := len(a.reconcileCh); got != 0 {
		t.Errorf("queued reconciles = %d, want 0", got)
	}
	for _, want := range []string{"configuration reload failed, keeping the running configuration", `error="load configuration: boom"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q does not contain %q", buf.String(), want)
		}
	}
}

func TestReloadWithoutLoader(t *testing.T) {
	m := withTestMetrics(t)
	captureSlog(t)
	a, _ := reloadAgent(t, loadedConfig(t), nil)

	if _, err := a.reload(); !errors.Is(err, errReloadUnavailable) {
		t.Errorf("reload() error = %v, want errReloadUnavailable", err)
	}
	a.handleReload()
	if got := counterValue(t, m, "ovn_network_agent_config_reload_total", "outcome", "error"); got != 1 {
		t.Errorf("config_reload_total{error} = %v, want 1", got)
	}
}

func TestReloadRejectsInvalidFile(t *testing.T) {
	running := loadedConfig(t)
	path := reloadConfigFile(t, "reconcile_interval: 0s\n")
	a, _ := reloadAgent(t, running, func() (Config, error) { return loadConfig(fullModeArgs("--config", path)) })

	_, err := a.reload()
	if err == nil || !strings.Contains(err.Error(), "invalid reconcile-interval") {
		t.Fatalf("reload() error = %v, want one containing %q", err, "invalid reconcile-interval")
	}
	if !reflect.DeepEqual(a.cfg, running) {
		t.Errorf("config changed on a failed reload: %+v", a.cfg)
	}
}

func TestHandleReloadWarnsAndKeepsRestartOnlyKey(t *testing.T) {
	withTestMetrics(t)
	keepLogLevel(t)
	buf := captureSlog(t)
	running := loadedConfig(t)
	next := loadedConfig(t, "--bridge-dev", "br-other", "--log-level", "debug")
	a, _ := reloadAgent(t, running, returning(next))

	a.handleReload()

	for _, want := range []string{"configuration change needs a restart, keeping the running value", "key=bridge_dev"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log %q does not contain %q", buf.String(), want)
		}
	}
	if a.cfg.BridgeDev != running.BridgeDev {
		t.Errorf("BridgeDev = %q, want the running %q", a.cfg.BridgeDev, running.BridgeDev)
	}
	if got := logLevel.Level(); got != slog.LevelDebug {
		t.Errorf("log level = %v, want debug", got)
	}
}

func TestReloadRenamesFRRPrefixList(t *testing.T) {
	const showOld = "show ip prefix-list OLD json"

	t.Run("empties the previous list", func(t *testing.T) {
		withTestMetrics(t)
		captureSlog(t)
		a, rec := reloadAgent(t, loadedConfig(t, "--frr-prefix-list", "OLD"),
			returning(loadedConfig(t, "--frr-prefix-list", "NEW")))
		rec.on([]string{"vtysh", "-c", showOld},
			frrPrefixListJSON("OLD", prefixListEntry{Seq: 5, Network: "198.51.100.0/24"}), nil)

		a.handleReload()

		want := [][]string{
			{"vtysh", "-c", showOld},
			{"vtysh", "-c", "conf t", "-c", "no ip prefix-list OLD seq 5 permit 198.51.100.0/24 ge 32 le 32", "-c", "end"},
		}
		if !reflect.DeepEqual(rec.calls, want) {
			t.Errorf("vtysh calls = %q, want %q", rec.calls, want)
		}
		if got := a.routing.cfg.FRRPrefixList; got != "NEW" {
			t.Errorf("FRRPrefixList = %q, want NEW", got)
		}
	})

	t.Run("no previous list", func(t *testing.T) {
		withTestMetrics(t)
		captureSlog(t)
		a, rec := reloadAgent(t, loadedConfig(t, "--frr-prefix-list", ""),
			returning(loadedConfig(t, "--frr-prefix-list", "NEW")))

		a.handleReload()

		if len(rec.calls) != 0 {
			t.Errorf("vtysh calls = %q, want none", rec.calls)
		}
		if got := a.routing.cfg.FRRPrefixList; got != "NEW" {
			t.Errorf("FRRPrefixList = %q, want NEW", got)
		}
	})

	t.Run("emptying fails", func(t *testing.T) {
		m := withTestMetrics(t)
		buf := captureSlog(t)
		a, rec := reloadAgent(t, loadedConfig(t, "--frr-prefix-list", "OLD"),
			returning(loadedConfig(t, "--frr-prefix-list", "NEW")))
		rec.on([]string{"vtysh", "-c", showOld}, "", errors.New("vtysh down"))

		a.handleReload()

		if !strings.Contains(buf.String(), "failed to empty the previous FRR prefix-list") {
			t.Errorf("log %q does not report the failed emptying", buf.String())
		}
		if got := counterValue(t, m, "ovn_network_agent_config_reload_total", "outcome", "success"); got != 1 {
			t.Errorf("config_reload_total{success} = %v, want 1", got)
		}
		if got := a.routing.cfg.FRRPrefixList; got != "NEW" {
			t.Errorf("FRRPrefixList = %q, want NEW", got)
		}
	})
}

func TestReloadWithdrawsDroppedVIPs(t *testing.T) {
	running := loadedConfig(t, "--config", reloadConfigFile(t, pfYAML("198.51.100.30", "198.51.100.31")))
	dropped := loadedConfig(t, "--config", reloadConfigFile(t, pfYAML("198.51.100.30")))

	t.Run("withdraws the dropped VIP", func(t *testing.T) {
		withTestMetrics(t)
		captureSlog(t)
		a, _ := reloadAgent(t, running, returning(dropped))
		var calls [][]string
		a.routing.withdrawVIPsHook = func(vips []string) error {
			calls = append(calls, vips)
			return nil
		}

		a.handleReload()

		if want := [][]string{{"198.51.100.31"}}; !reflect.DeepEqual(calls, want) {
			t.Errorf("withdrawn = %v, want %v", calls, want)
		}
	})

	t.Run("a withdrawal failure is logged", func(t *testing.T) {
		m := withTestMetrics(t)
		buf := captureSlog(t)
		a, _ := reloadAgent(t, running, returning(dropped))
		a.routing.withdrawVIPsHook = func([]string) error { return errors.New("netlink down") }

		a.handleReload()

		if !strings.Contains(buf.String(), "failed to withdraw VIP addresses removed by the reload") {
			t.Errorf("log %q does not report the failed withdrawal", buf.String())
		}
		if got := counterValue(t, m, "ovn_network_agent_config_reload_total", "outcome", "success"); got != 1 {
			t.Errorf("config_reload_total{success} = %v, want 1", got)
		}
	})

	t.Run("unchanged port_forwards withdraw nothing", func(t *testing.T) {
		withTestMetrics(t)
		captureSlog(t)
		next := running
		next.LogLevel = "warn"
		keepLogLevel(t)
		a, _ := reloadAgent(t, running, returning(next))
		a.routing.withdrawVIPsHook = func([]string) error {
			t.Error("withdrawVIPs called, want no call")
			return nil
		}

		a.handleReload()
	})
}

func TestWithdrawVIPAddressesDryRunAndEmpty(t *testing.T) {
	rm := &RouteManager{cfg: Config{DryRun: true, PortForwardDev: "loopback1"}}
	buf := captureSlog(t)

	if err := rm.WithdrawVIPAddresses([]string{"198.51.100.31"}); err != nil {
		t.Errorf("WithdrawVIPAddresses() under dry-run error = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "[dry-run] would remove VIP address") {
		t.Errorf("log %q does not contain the dry-run line", buf.String())
	}

	rm.cfg.DryRun = false
	if err := rm.WithdrawVIPAddresses(nil); err != nil {
		t.Errorf("WithdrawVIPAddresses(nil) error = %v, want nil", err)
	}
}

func TestReloadDisablingStaleCleanupResetsTracker(t *testing.T) {
	m := withTestMetrics(t)
	captureSlog(t)
	a, _ := reloadAgent(t, loadedConfig(t), returning(loadedConfig(t, "--stale-chassis-grace-period", "0")))
	a.missingChassis["node-gone"] = time.Now()
	setMissingChassis(1)

	a.handleReload()

	if len(a.missingChassis) != 0 {
		t.Errorf("missingChassis = %v, want empty", a.missingChassis)
	}
	if got := gaugeValue(t, m, "ovn_network_agent_missing_chassis"); got != 0 {
		t.Errorf("missing_chassis = %v, want 0", got)
	}
}

func TestReloadAppliesDrainSettleDelay(t *testing.T) {
	running := loadedConfig(t)
	next := loadedConfig(t, "--drain-settle-delay", "2s")

	t.Run("with an OVN client", func(t *testing.T) {
		withTestMetrics(t)
		captureSlog(t)
		a, _ := reloadAgent(t, running, returning(next))
		a.ovn = NewOVNClient(running, func() {})

		a.handleReload()

		if got := a.ovn.cfg.DrainSettleDelay; got != 2*time.Second {
			t.Errorf("OVN client DrainSettleDelay = %v, want 2s", got)
		}
	})

	t.Run("port-forward-only agent without an OVN client", func(t *testing.T) {
		withTestMetrics(t)
		captureSlog(t)
		a, _ := reloadAgent(t, running, returning(next))

		a.handleReload() // must not dereference the nil a.ovn

		if got := a.cfg.DrainSettleDelay; got != 2*time.Second {
			t.Errorf("DrainSettleDelay = %v, want 2s", got)
		}
	})
}

func TestReloadRaisesLogLevel(t *testing.T) {
	withTestMetrics(t)
	keepLogLevel(t)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	setupLogging("info")

	a, _ := reloadAgent(t, loadedConfig(t, "--log-level", "info"),
		returning(loadedConfig(t, "--log-level", "debug")))
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug enabled before the reload")
	}

	a.handleReload()

	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Error("debug not enabled after reloading log_level: debug")
	}
}

func TestReloadRotatesClientCertificate(t *testing.T) {
	setup := func(t *testing.T) (a *Agent, certPath, keyPath string, before *tls.Certificate) {
		t.Helper()
		withTestMetrics(t)
		certPath, keyPath = writeTestTLSFiles(t)
		args := []string{"--ovn-ssl-cert", certPath, "--ovn-ssl-key", keyPath}
		running := loadedConfig(t, args...)
		a, _ = reloadAgent(t, running, func() (Config, error) { return loadConfig(fullModeArgs(args...)) })
		return a, certPath, keyPath, running.OVNClientCert.load()
	}

	t.Run("rotated in place", func(t *testing.T) {
		buf := captureSlog(t)
		a, certPath, keyPath, before := setup(t)
		holder := a.cfg.OVNClientCert
		newCert, newKey := writeTestTLSFiles(t)
		copyPEM(t, newCert, certPath)
		copyPEM(t, newKey, keyPath)
		want, err := tls.LoadX509KeyPair(newCert, newKey)
		if err != nil {
			t.Fatalf("load new pair: %v", err)
		}

		a.handleReload()

		if a.cfg.OVNClientCert != holder {
			t.Fatal("holder replaced, want the running holder updated in place")
		}
		got := a.cfg.OVNClientCert.load()
		if bytes.Equal(got.Certificate[0], before.Certificate[0]) || !bytes.Equal(got.Certificate[0], want.Certificate[0]) {
			t.Error("holder does not serve the rotated certificate")
		}
		if !strings.Contains(buf.String(), "OVN client certificate reloaded") {
			t.Errorf("log %q does not report the rotation", buf.String())
		}
	})

	t.Run("invalid PEM keeps the old certificate", func(t *testing.T) {
		captureSlog(t)
		a, certPath, _, before := setup(t)
		if err := os.WriteFile(certPath, []byte("not a pem file\n"), 0o600); err != nil {
			t.Fatalf("write garbage: %v", err)
		}

		_, err := a.reload()
		if err == nil || !strings.Contains(err.Error(), "load ovn-ssl-cert") {
			t.Fatalf("reload() error = %v, want one containing %q", err, "load ovn-ssl-cert")
		}
		if a.cfg.OVNClientCert.load() != before {
			t.Error("holder changed on a failed reload")
		}
	})

	t.Run("world-readable key keeps the old certificate", func(t *testing.T) {
		captureSlog(t)
		a, _, keyPath, before := setup(t)
		if err := os.Chmod(keyPath, 0o644); err != nil {
			t.Fatalf("chmod key: %v", err)
		}

		_, err := a.reload()
		if err == nil || !strings.Contains(err.Error(), "must not be group- or world-accessible") {
			t.Fatalf("reload() error = %v, want the key-mode rejection", err)
		}
		if a.cfg.OVNClientCert.load() != before {
			t.Error("holder changed on a failed reload")
		}
	})
}
