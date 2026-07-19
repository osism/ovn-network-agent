//go:build integration

package testenv

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig captures the config-file fields the integration harness uses
// to run the agent. Zero-valued fields fall back to AgentConfig defaults
// applied by Defaults().
type AgentConfig struct {
	OVNNBRemote string `yaml:"ovn_nb_remote"`
	OVNSBRemote string `yaml:"ovn_sb_remote"`
	// OVNSSLCA/OVNSSLCert/OVNSSLKey are the PEM file paths for ssl: OVN
	// remotes (mirrors the agent's ovn_ssl_ca/ovn_ssl_cert/ovn_ssl_key).
	OVNSSLCA      string   `yaml:"ovn_ssl_ca,omitempty"`
	OVNSSLCert    string   `yaml:"ovn_ssl_cert,omitempty"`
	OVNSSLKey     string   `yaml:"ovn_ssl_key,omitempty"`
	BridgeDev     string   `yaml:"bridge_dev"`
	BridgeIP      string   `yaml:"bridge_ip,omitempty"`
	VRFName       string   `yaml:"vrf_name"`
	VethNexthop   string   `yaml:"veth_nexthop"`
	NetworkCIDRs  []string `yaml:"network_cidrs"`
	FRRPrefixList string   `yaml:"frr_prefix_list"`
	LogLevel      string   `yaml:"log_level"`

	// GatewayPort opts the agent into single-port mode: only the named
	// chassisredirect Port_Binding is tracked, all others are ignored. Used
	// by legacy single-router deployments. Empty value (the default) enables
	// the agent's multi-router mode.
	GatewayPort string `yaml:"gateway_port,omitempty"`

	// Pointers so we can distinguish "unset" from "explicit false".
	DryRun            *bool `yaml:"dry_run,omitempty"`
	CleanupOnShutdown *bool `yaml:"cleanup_on_shutdown,omitempty"`
	DrainOnShutdown   *bool `yaml:"drain_on_shutdown,omitempty"`
	VethLeakEnabled   *bool `yaml:"veth_leak_enabled,omitempty"`

	ReconcileInterval       string `yaml:"reconcile_interval,omitempty"`
	StaleChassisGracePeriod string `yaml:"stale_chassis_grace_period,omitempty"`
	DrainTimeout            string `yaml:"drain_timeout,omitempty"`
	DrainSettleDelay        string `yaml:"drain_settle_delay,omitempty"`

	// MetricsListen is the address the agent's Prometheus /metrics endpoint
	// binds to. Empty disables the endpoint (the agent's default). Scenario
	// tests should pick a free loopback port with FreeLoopbackAddr so parallel
	// runs do not collide.
	MetricsListen string `yaml:"metrics_listen,omitempty"`

	// Port forwarding (DNAT). PortForwards is the list of VIPs and rules; the
	// agent infers PortForwardEnabled from len > 0. PortForwardL3mdevAccept
	// flips a *global* sysctl when true — scenario tests that set it must use
	// SaveSysctl/RestoreSysctl to avoid leaking state.
	PortForwardDev          string                  `yaml:"port_forward_dev,omitempty"`
	PortForwardL3mdevAccept *bool                   `yaml:"port_forward_l3mdev_accept,omitempty"`
	PortForwardCTZone       *int                    `yaml:"port_forward_ct_zone,omitempty"`
	PortForwards            []PortForwardVIPFixture `yaml:"port_forwards,omitempty"`

	// Routing-table IDs for each writer plane. Mirrors the agent's
	// route_table_id / veth_leak_table_id / port_forward_table_id options.
	// Pointer so a test can opt into a non-default value without forcing
	// every other test to populate them.
	RouteTableID       *int `yaml:"route_table_id,omitempty"`
	VethLeakTableID    *int `yaml:"veth_leak_table_id,omitempty"`
	PortForwardTableID *int `yaml:"port_forward_table_id,omitempty"`

	// ExtraEnv is appended to the agent subprocess's environment after
	// os.Environ(). Scenario tests use it to prepend a shim directory to
	// PATH (see WithFailingTool) — later "KEY=value" entries override
	// earlier ones, which is how PATH overrides work.
	ExtraEnv []string `yaml:"-"`
}

// PortForwardVIPFixture mirrors the agent's PortForwardVIP for scenario
// fixtures. It cannot import from main, so the YAML keys must stay in lock
// step with config.go's PortForwardVIP / PortForwardRule.
type PortForwardVIPFixture struct {
	VIP               string                   `yaml:"vip"`
	ManageVIP         bool                     `yaml:"manage_vip,omitempty"`
	Masquerade        bool                     `yaml:"masquerade,omitempty"`
	HairpinMasquerade bool                     `yaml:"hairpin_masquerade,omitempty"`
	RouterMasquerade  bool                     `yaml:"router_masquerade,omitempty"`
	Rules             []PortForwardRuleFixture `yaml:"rules"`
}

// PortForwardRuleFixture mirrors the agent's PortForwardRule. Masquerade is a
// pointer to distinguish "inherit from VIP" (nil) from explicit true/false.
type PortForwardRuleFixture struct {
	Proto      string   `yaml:"proto"`
	Port       int      `yaml:"port"`
	DestAddr   string   `yaml:"dest_addr,omitempty"`
	DestAddrs  []string `yaml:"dest_addrs,omitempty"`
	DestPort   int      `yaml:"dest_port,omitempty"`
	Masquerade *bool    `yaml:"masquerade,omitempty"`
}

// Defaults returns an AgentConfig wired for the local test stack:
// TCP NB on 6641, TCP SB on 6642, FRR prefix-list disabled to avoid
// touching FRR config that may not exist.
func Defaults() AgentConfig {
	off := false
	return AgentConfig{
		OVNNBRemote:       "tcp:127.0.0.1:6641",
		OVNSBRemote:       "tcp:127.0.0.1:6642",
		BridgeDev:         DefaultBridgeDev,
		VRFName:           DefaultVRFName,
		VethNexthop:       "169.254.0.1",
		FRRPrefixList:     "",
		LogLevel:          "debug",
		DrainOnShutdown:   &off,
		ReconcileInterval: "5s",
	}
}

// FastDefaults is Defaults() with a tightened reconcile interval. Use it for
// scenarios that need to observe drift recovery, stale-chassis cleanup, or
// any other periodic behaviour without paying the full 5s production tick.
// Tests that must mirror production timing should keep using Defaults().
func FastDefaults() AgentConfig {
	cfg := Defaults()
	cfg.ReconcileInterval = "2s"
	return cfg
}

// AgentProc is a handle to a running agent subprocess.
type AgentProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	stderr  io.ReadCloser
	configF string

	// readyCh is closed when "agent running" appears on stderr.
	readyCh chan struct{}
	// exitCh receives the process exit error once cmd.Wait() returns.
	exitCh chan error

	mu      sync.Mutex
	logBuf  strings.Builder
	stopped bool
}

// RunAgent writes cfg to a temp YAML file, execs the agent binary, and
// returns a handle. The process is killed on test cleanup if Stop has
// not already been called.
func RunAgent(t *testing.T, cfg AgentConfig) *AgentProc {
	t.Helper()

	configPath := writeTempConfig(t, cfg)

	bin := AgentBinary(t)
	cmd := exec.Command(bin, "--config", configPath)
	cmd.Env = append(os.Environ(), "GOTRACEBACK=all")
	// A -cover-built agent (see `make build-integration`) writes per-process
	// coverage counter files to GOCOVERDIR on clean (SIGTERM) exit. os.Environ()
	// already carries it when the CI job sets it, but propagate it explicitly so
	// the contract is visible at the spawn site. A SIGKILL'd agent loses its
	// counters, which is acceptable best-effort.
	if dir := os.Getenv("GOCOVERDIR"); dir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+dir)
	}
	// ExtraEnv must REPLACE existing entries with the same key, not append.
	// Linux's getenv() returns the first matching entry, so a trailing
	// "PATH=<shim>:..." would be ignored — the agent would still resolve
	// tools via the original PATH and the WithFailingTool shim would never
	// fire. mergeEnv splices each "KEY=value" in over its prior occurrence.
	cmd.Env = mergeEnv(cmd.Env, cfg.ExtraEnv)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	cmd.Stdout = os.Stdout

	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Logf("agent started pid=%d binary=%s config=%s", cmd.Process.Pid, bin, configPath)

	p := &AgentProc{
		t:       t,
		cmd:     cmd,
		stderr:  stderr,
		configF: configPath,
		readyCh: make(chan struct{}),
		exitCh:  make(chan error, 1),
	}

	go p.scanStderr()

	go func() {
		p.exitCh <- cmd.Wait()
	}()

	t.Cleanup(func() {
		// Best-effort kill if the test forgot to call Stop. We don't
		// fail the cleanup — that masks the real test failure.
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if !stopped {
			_ = p.cmd.Process.Signal(syscall.SIGKILL)
			<-p.exitCh
		}
	})

	return p
}

// WithAgent is the convenience composition of RunAgent + WaitReady + a
// deferred Stop registered via t.Cleanup. It is the right helper for the
// common shape of "spin up an agent against this config, run a scenario,
// let SIGTERM-on-exit do the cleanup".
//
// Tests that need to assert on Stop() ordering or timing (drain-on-shutdown,
// SIGTERM cleanup verification) should keep using RunAgent + manual Stop —
// WithAgent's cleanup runs after t returns, which is too late for those
// assertions. The cleanup tolerates a prior manual Stop because AgentProc
// dedupes on its `stopped` flag.
func WithAgent(t *testing.T, cfg AgentConfig) *AgentProc {
	t.Helper()
	a := RunAgent(t, cfg)

	readyCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.WaitReady(readyCtx); err != nil {
		t.Fatalf("agent did not become ready: %v", err)
	}

	t.Cleanup(func() {
		if err := a.Stop(15 * time.Second); err != nil {
			t.Errorf("agent stop in WithAgent cleanup: %v", err)
		}
	})

	return a
}

// scanStderr forwards agent stderr to t.Log, buffers it for diagnostics,
// and signals readyCh when the "agent running" line appears.
func (p *AgentProc) scanStderr() {
	scanner := bufio.NewScanner(p.stderr)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	readyClosed := false
	for scanner.Scan() {
		line := scanner.Text()
		p.t.Logf("agent: %s", line)
		p.mu.Lock()
		p.logBuf.WriteString(line)
		p.logBuf.WriteByte('\n')
		p.mu.Unlock()
		if !readyClosed && strings.Contains(line, "agent running") {
			close(p.readyCh)
			readyClosed = true
		}
	}
	if !readyClosed {
		// stderr closed before we ever saw "agent running" — unblock
		// any WaitReady caller so they don't hang on a dead process.
		close(p.readyCh)
	}
}

// WaitReady blocks until the agent logs "agent running" (which fires
// after Connect + initial reconcile) or ctx is cancelled.
func (p *AgentProc) WaitReady(ctx context.Context) error {
	select {
	case <-p.readyCh:
		// readyCh may close because the process died early; surface that.
		select {
		case err := <-p.exitCh:
			p.exitCh <- err
			return fmt.Errorf("agent exited before becoming ready: %w (logs: %s)", err, p.LogTail(20))
		default:
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop sends SIGTERM and waits up to timeout for clean exit. If the
// process does not exit in time, it is killed with SIGKILL.
func (p *AgentProc) Stop(timeout time.Duration) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal SIGTERM: %w", err)
	}

	select {
	case err := <-p.exitCh:
		if err != nil {
			// Exit due to signal-induced shutdown is fine; the agent
			// returns nil from Run() when ctx is cancelled, so a
			// graceful SIGTERM yields exit code 0.
			return fmt.Errorf("agent exited with error: %w", err)
		}
		return nil
	case <-time.After(timeout):
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		<-p.exitCh
		return fmt.Errorf("agent did not exit within %s, killed", timeout)
	}
}

// Alive reports whether the agent subprocess is still running. It does so
// non-destructively by peeking at exitCh — Wait() has been started in
// RunAgent, so exitCh closes (with the exit error) the moment the process
// terminates. Scenarios that need to confirm "the agent did NOT exit after
// some external perturbation" should poll this rather than re-signalling.
func (p *AgentProc) Alive() bool {
	select {
	case err := <-p.exitCh:
		// Put the value back so a subsequent Stop / Alive sees it too.
		p.exitCh <- err
		return false
	default:
		return true
	}
}

// LogTail returns the last n lines of agent stderr captured so far.
func (p *AgentProc) LogTail(n int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := strings.Split(strings.TrimRight(p.logBuf.String(), "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// writeTempConfig serialises cfg to a temp YAML file in t.TempDir(). It uses
// yaml.v3 with explicit handling for fields whose semantics differ between
// "unset" and "empty string". In particular, frr_prefix_list always emits a
// quoted value (the agent treats an absent key as "use the default
// ANNOUNCED-NETWORKS prefix list", but tests usually want to disable it
// outright by passing an empty string).
func writeTempConfig(t *testing.T, cfg AgentConfig) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")

	doc := map[string]any{}
	put := func(k, v string) {
		if v != "" {
			doc[k] = v
		}
	}
	putBool := func(k string, v *bool) {
		if v != nil {
			doc[k] = *v
		}
	}
	putInt := func(k string, v *int) {
		if v != nil {
			doc[k] = *v
		}
	}

	put("ovn_nb_remote", cfg.OVNNBRemote)
	put("ovn_sb_remote", cfg.OVNSBRemote)
	put("ovn_ssl_ca", cfg.OVNSSLCA)
	put("ovn_ssl_cert", cfg.OVNSSLCert)
	put("ovn_ssl_key", cfg.OVNSSLKey)
	put("bridge_dev", cfg.BridgeDev)
	put("bridge_ip", cfg.BridgeIP)
	put("vrf_name", cfg.VRFName)
	put("veth_nexthop", cfg.VethNexthop)
	// Manual NetworkCIDRs override auto-discovery from Logical_Router_Port.networks
	// and become the operator's only knob for opting some networks out of BGP
	// announcement: an external IP outside this set MUST NOT install kernel/FRR
	// routes. The agent's YAML key is `network_cidr` (StringOrSlice), so emit
	// that even though the testenv struct tag is the more readable plural form.
	if len(cfg.NetworkCIDRs) > 0 {
		doc["network_cidr"] = cfg.NetworkCIDRs
	}
	// frr_prefix_list intentionally always emits, allowing "" to disable.
	doc["frr_prefix_list"] = cfg.FRRPrefixList
	put("log_level", cfg.LogLevel)
	put("gateway_port", cfg.GatewayPort)
	put("reconcile_interval", cfg.ReconcileInterval)
	put("stale_chassis_grace_period", cfg.StaleChassisGracePeriod)
	put("drain_timeout", cfg.DrainTimeout)
	put("drain_settle_delay", cfg.DrainSettleDelay)
	put("metrics_listen", cfg.MetricsListen)
	putBool("dry_run", cfg.DryRun)
	putBool("cleanup_on_shutdown", cfg.CleanupOnShutdown)
	putBool("drain_on_shutdown", cfg.DrainOnShutdown)
	putBool("veth_leak_enabled", cfg.VethLeakEnabled)

	put("port_forward_dev", cfg.PortForwardDev)
	putBool("port_forward_l3mdev_accept", cfg.PortForwardL3mdevAccept)
	putInt("port_forward_ct_zone", cfg.PortForwardCTZone)
	if len(cfg.PortForwards) > 0 {
		doc["port_forwards"] = cfg.PortForwards
	}

	putInt("route_table_id", cfg.RouteTableID)
	putInt("veth_leak_table_id", cfg.VethLeakTableID)
	putInt("port_forward_table_id", cfg.PortForwardTableID)

	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// mergeEnv returns base with each "KEY=value" entry from overrides spliced
// in over its prior occurrence. Keys not yet present are appended at the
// end. Unlike a naive `append`, this guarantees the override wins
// regardless of whether libc's getenv() returns the first or last matching
// entry — Linux libc returns the first, which is exactly the trap
// WithFailingTool used to fall into.
func mergeEnv(base, overrides []string) []string {
	out := make([]string, len(base))
	copy(out, base)
	for _, entry := range overrides {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		replaced := false
		for i, existing := range out {
			eKey := existing
			if j := strings.IndexByte(existing, '='); j >= 0 {
				eKey = existing[:j]
			}
			if eKey == key {
				out[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}
	return out
}
