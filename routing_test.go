package main

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// AddFRRRoute, DelFRRRoute and HasFRRRoute are single-IP convenience wrappers
// used only by the tests below. They live here rather than in routing.go so
// production code carries no unused API — and, in HasFRRRoute's case, no
// human-readable `show ip route` text parsing outside the test suite.
func (rm *RouteManager) AddFRRRoute(ip string) error {
	return rm.AddFRRRoutes([]string{ip})
}

func (rm *RouteManager) DelFRRRoute(ip string) error {
	return rm.DelFRRRoutes([]string{ip})
}

func (rm *RouteManager) HasFRRRoute(ip string) bool {
	if err := validateIP(ip); err != nil {
		return false
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip route vrf %s %s/32", rm.cfg.VRFName, ip))
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "static")
}

// frrStaticRoutesJSON renders an FRR `show ip route vrf <vrf> static json`
// document for the given /32 IPs, all resolved via the given next-hop. It is
// the structured stand-in for the human-readable `show ip route` table the
// agent used to parse.
func frrStaticRoutesJSON(nexthop string, ips ...string) string {
	entries := make([]string, 0, len(ips))
	for _, ip := range ips {
		entries = append(entries, fmt.Sprintf(
			`%q:[{"prefix":%q,"protocol":"static","selected":true,"installed":true,`+
				`"nexthops":[{"ip":%q,"active":true,"fib":true,"interfaceName":"veth-default"}]}]`,
			ip+"/32", ip+"/32", nexthop))
	}
	return "{" + strings.Join(entries, ",") + "}"
}

// frrPrefixListDoc renders one FRR daemon's `show ip prefix-list <name> json`
// document. FRR nests the list under the daemon's own protocol name, so the
// shape is {"<daemon>":{"<name>":{"addressFamily":"IPv4","entries":[…]}}} with
// one "permit <network> ge 32 le 32" entry per (seq, network) pair.
func frrPrefixListDoc(daemon, name string, entries ...prefixListEntry) string {
	rows := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Exact {
			// FRR omits minimumPrefixLength/maximumPrefixLength entirely
			// for an exact entry (verified against the shipped FRR 8.4.4).
			rows = append(rows, fmt.Sprintf(
				`{"sequenceNumber":%d,"type":"permit","prefix":%q}`,
				e.Seq, e.Network))
			continue
		}
		rows = append(rows, fmt.Sprintf(
			`{"sequenceNumber":%d,"type":"permit","prefix":%q,`+
				`"minimumPrefixLength":32,"maximumPrefixLength":32}`,
			e.Seq, e.Network))
	}
	return fmt.Sprintf(`{%q:{%q:{"addressFamily":"IPv4","entries":[%s]}}}`,
		daemon, name, strings.Join(rows, ","))
}

// frrPrefixListJSON renders the full body vtysh returns for a list configured
// through it: zebra and bgpd both store the list, so their two documents are
// concatenated with identical entries. This is the real shape
// ListFRRPrefixListEntries reads.
func frrPrefixListJSON(name string, entries ...prefixListEntry) string {
	return frrPrefixListDoc("ZEBRA", name, entries...) +
		frrPrefixListDoc("BGP", name, entries...)
}

// vtyshRecorder captures calls to RouteManager.runVtysh for assertion. It
// mirrors ovsRecorder in ovs_test.go: it returns canned responses keyed by
// the full joined command line and falls back to (nil, nil) for unmatched
// commands.
type vtyshRecorder struct {
	calls     [][]string
	responses map[string]ovsResponse
}

func newVtyshRecorder() *vtyshRecorder {
	return &vtyshRecorder{responses: map[string]ovsResponse{}}
}

func (r *vtyshRecorder) on(args []string, out string, err error) {
	r.responses[strings.Join(args, " ")] = ovsResponse{out: []byte(out), err: err}
}

func (r *vtyshRecorder) hook() ovsExecFunc {
	return func(cmd *exec.Cmd) ([]byte, error) {
		r.calls = append(r.calls, append([]string{}, cmd.Args...))
		if resp, ok := r.responses[strings.Join(cmd.Args, " ")]; ok {
			return resp.out, resp.err
		}
		return nil, nil
	}
}

// TestIsNoSuchRoute pins the errno contract: the kernel reports a missing route
// as ESRCH, and the check must recognise it through wrapping — while NOT
// matching an error that merely renders similarly. Matching the strerror text
// instead would break on any host that words the message differently.
func TestIsNoSuchRoute(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bare ESRCH", syscall.ESRCH, true},
		{"wrapped ESRCH", fmt.Errorf("netlink: del route: %w", syscall.ESRCH), true},
		{"doubly wrapped ESRCH", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", syscall.ESRCH)), true},
		{"a different errno", syscall.EPERM, false},
		{"ENOENT is a missing rule, not a missing route", syscall.ENOENT, false},
		// A plain error whose text happens to read like the old substring
		// match must NOT be treated as "already gone".
		{"text-only lookalike", errors.New("no such process"), false},
		{"unrelated error", errors.New("permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNoSuchRoute(tt.err); got != tt.want {
				t.Errorf("isNoSuchRoute(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestWarnIfVtyshMissing(t *testing.T) {
	t.Run("warns at WARN level when vtysh is absent", func(t *testing.T) {
		buf := captureSlog(t)
		warnIfVtyshMissing(func(string) (string, error) {
			return "", exec.ErrNotFound
		})
		out := buf.String()
		if !strings.Contains(out, "vtysh not found on PATH") {
			t.Fatalf("expected a warning about missing vtysh, got: %q", out)
		}
		if !strings.Contains(out, "level=WARN") {
			t.Fatalf("expected the warning at WARN level, got: %q", out)
		}
	})

	t.Run("silent when vtysh resolves", func(t *testing.T) {
		buf := captureSlog(t)
		warnIfVtyshMissing(func(name string) (string, error) {
			return "/usr/bin/" + name, nil
		})
		if buf.Len() != 0 {
			t.Fatalf("expected no output when vtysh resolves, got: %q", buf.String())
		}
	})
}

func TestNewRouteManager(t *testing.T) {
	cfg := Config{
		BridgeDev:   "br-ex",
		VRFName:     "vrf-provider",
		VethNexthop: "169.254.0.1",
	}

	rm := NewRouteManager(cfg)

	if rm.cfg.BridgeDev != "br-ex" {
		t.Errorf("bridgeDev = %q, want %q", rm.cfg.BridgeDev, "br-ex")
	}
	if rm.cfg.VRFName != "vrf-provider" {
		t.Errorf("vrfName = %q, want %q", rm.cfg.VRFName, "vrf-provider")
	}
	if rm.cfg.VethNexthop != "169.254.0.1" {
		t.Errorf("vethNexthop = %q, want %q", rm.cfg.VethNexthop, "169.254.0.1")
	}
	if rm.cfg.RouteTableID != 0 {
		t.Errorf("routeTableID = %d, want 0", rm.cfg.RouteTableID)
	}
	if rm.cfg.DryRun {
		t.Error("dryRun should be false by default")
	}
}

func TestNewRouteManagerWithTableID(t *testing.T) {
	cfg := Config{
		BridgeDev:    "br-ex",
		VRFName:      "vrf-provider",
		VethNexthop:  "169.254.0.1",
		RouteTableID: 100,
	}

	rm := NewRouteManager(cfg)

	if rm.cfg.RouteTableID != 100 {
		t.Errorf("routeTableID = %d, want 100", rm.cfg.RouteTableID)
	}
}

func TestDryRunBridgeIP(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", DryRun: true}}

	if err := rm.EnsureBridgeIP("169.254.169.254"); err != nil {
		t.Errorf("EnsureBridgeIP() in dry-run should not error, got: %v", err)
	}
	if err := rm.RemoveBridgeIP("169.254.169.254"); err != nil {
		t.Errorf("RemoveBridgeIP() in dry-run should not error, got: %v", err)
	}
}

func TestDryRunOVSFlows(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", DryRun: true}}

	if err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}}); err != nil {
		t.Errorf("EnsureSegments() in dry-run should not error, got: %v", err)
	}
	if err := rm.RemoveOVSFlows(); err != nil {
		t.Errorf("RemoveOVSFlows() in dry-run should not error, got: %v", err)
	}
}

func TestNewRouteManagerDryRun(t *testing.T) {
	cfg := Config{
		BridgeDev:   "br-ex",
		VRFName:     "vrf-provider",
		VethNexthop: "169.254.0.1",
		DryRun:      true,
	}

	rm := NewRouteManager(cfg)

	if !rm.cfg.DryRun {
		t.Error("dryRun should be true when config has DryRun=true")
	}
}

func TestDryRunFRRRoutes(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}

	if err := rm.AddFRRRoute("10.0.0.1"); err != nil {
		t.Errorf("AddFRRRoute() in dry-run should not error, got: %v", err)
	}
	if err := rm.DelFRRRoute("10.0.0.1"); err != nil {
		t.Errorf("DelFRRRoute() in dry-run should not error, got: %v", err)
	}
}

func TestDryRunFRRRoutesBatch(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if err := rm.AddFRRRoutes(ips); err != nil {
		t.Errorf("AddFRRRoutes() in dry-run should not error, got: %v", err)
	}
	if err := rm.DelFRRRoutes(ips); err != nil {
		t.Errorf("DelFRRRoutes() in dry-run should not error, got: %v", err)
	}
}

func TestFRRRoutesBatchEmpty(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}}

	if err := rm.AddFRRRoutes(nil); err != nil {
		t.Errorf("AddFRRRoutes(nil) should be no-op, got: %v", err)
	}
	if err := rm.DelFRRRoutes(nil); err != nil {
		t.Errorf("DelFRRRoutes(nil) should be no-op, got: %v", err)
	}
	if err := rm.AddFRRRoutes([]string{}); err != nil {
		t.Errorf("AddFRRRoutes([]) should be no-op, got: %v", err)
	}
	if err := rm.DelFRRRoutes([]string{}); err != nil {
		t.Errorf("DelFRRRoutes([]) should be no-op, got: %v", err)
	}
}

func TestDryRunRefreshBGP(t *testing.T) {
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", DryRun: true}}
	if err := rm.RefreshBGP(); err != nil {
		t.Errorf("RefreshBGP() in dry-run should not error, got: %v", err)
	}
}

func TestFRRRoutesBatchValidation(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}

	// AddFRRRoutes now skips invalid entries instead of rejecting the whole
	// batch, so a mixed list applies the valid IPs and returns nil. DelFRRRoutes
	// keeps fail-fast validation (its inputs are self-sourced from FRR output).
	mixed := []string{"10.0.0.1", "not-an-ip", "10.0.0.2"}
	if err := rm.AddFRRRoutes(mixed); err != nil {
		t.Errorf("AddFRRRoutes() with a mixed list should skip the invalid entry, got: %v", err)
	}
	if err := rm.DelFRRRoutes(mixed); err == nil {
		t.Error("DelFRRRoutes() with invalid IP should return error")
	}

	// A list of only-invalid entries is a no-op, not an error.
	if err := rm.AddFRRRoutes([]string{"10.0.0.1/32"}); err != nil {
		t.Errorf("AddFRRRoutes() with only invalid entries should be a no-op, got: %v", err)
	}
}

// TestAddFRRRoutesSkipsInvalidAndContinues proves a single malformed IP is
// dropped from the vtysh batch while the healthy IPs still install in one call
// (issue #158 test b, FRR half).
func TestAddFRRRoutesSkipsInvalidAndContinues(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
	if err := rm.AddFRRRoutes([]string{"10.0.0.1", "not-an-ip", "10.0.0.2"}); err != nil {
		t.Fatalf("AddFRRRoutes: unexpected error %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly one vtysh batch, got %d: %v", len(rec.calls), rec.calls)
	}
	joined := strings.Join(rec.calls[0], " ")
	if !strings.Contains(joined, "ip route 10.0.0.1/32 169.254.0.1") ||
		!strings.Contains(joined, "ip route 10.0.0.2/32 169.254.0.1") {
		t.Errorf("healthy IPs missing from batch: %v", rec.calls[0])
	}
	if strings.Contains(joined, "not-an-ip") {
		t.Errorf("invalid IP must not reach the vtysh batch: %v", rec.calls[0])
	}
}

// TestAddFRRRoutesContinuesPastFailedChunk proves a vtysh failure on the first
// chunk does not abort the remaining chunks: both are issued, and the returned
// error reflects the failed one (issue #158 test b — a failed chunk no longer
// aborts the rest).
func TestAddFRRRoutesContinuesPastFailedChunk(t *testing.T) {
	// frrBatchSize+1 IPs span exactly two chunks.
	ips := make([]string, 0, frrBatchSize+1)
	for i := 0; i < frrBatchSize+1; i++ {
		ips = append(ips, fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
	}

	var calls int
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: func(*exec.Cmd) ([]byte, error) {
		calls++
		if calls == 1 {
			return []byte("boom"), errors.New("vtysh failed")
		}
		return nil, nil
	}}
	err := rm.AddFRRRoutes(ips)
	if err == nil {
		t.Fatal("expected an aggregated error from the failed chunk, got nil")
	}
	if calls != 2 {
		t.Errorf("expected both chunks to be issued despite the first failing, got %d calls", calls)
	}
}

func TestNewRouteManagerVethLeak(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	cfg := Config{
		BridgeDev:            "br-ex",
		VRFName:              "vrf-provider",
		VethNexthop:          "169.254.0.1",
		VethLeakEnabled:      true,
		VethProviderIP:       "169.254.0.2",
		VethLeakTableID:      200,
		VethLeakRulePriority: 2000,
		NetworkFilters:       []*net.IPNet{cidr},
	}

	rm := NewRouteManager(cfg)

	if !rm.cfg.VethLeakEnabled {
		t.Error("vethLeakEnabled should be true")
	}
	if rm.cfg.VethProviderIP != "169.254.0.2" {
		t.Errorf("vethProviderIP = %q, want %q", rm.cfg.VethProviderIP, "169.254.0.2")
	}
	if rm.cfg.VethLeakTableID != 200 {
		t.Errorf("vethLeakTableID = %d, want 200", rm.cfg.VethLeakTableID)
	}
	if rm.cfg.VethLeakRulePriority != 2000 {
		t.Errorf("vethLeakRulePriority = %d, want 2000", rm.cfg.VethLeakRulePriority)
	}
	if len(rm.cfg.NetworkFilters) != 1 {
		t.Errorf("networkFilters length = %d, want 1", len(rm.cfg.NetworkFilters))
	}
}

func TestDryRunVethLeak(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", VethLeakEnabled: true, VethProviderIP: "169.254.0.2", VethLeakTableID: 200, DryRun: true}}

	if err := rm.SetupVethLeak(); err != nil {
		t.Errorf("SetupVethLeak() in dry-run should not error, got: %v", err)
	}
	if err := rm.TeardownVethLeak(); err != nil {
		t.Errorf("TeardownVethLeak() in dry-run should not error, got: %v", err)
	}
}

func TestDisabledVethLeak(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", VRFName: "vrf-provider", VethNexthop: "169.254.0.1", VethLeakEnabled: false}}

	if err := rm.SetupVethLeak(); err != nil {
		t.Errorf("SetupVethLeak() when disabled should not error, got: %v", err)
	}
	if err := rm.TeardownVethLeak(); err != nil {
		t.Errorf("TeardownVethLeak() when disabled should not error, got: %v", err)
	}
}

// frrConnectedRoutesJSON renders an FRR `show ip route vrf <vrf> connected json`
// document for the given prefixes, each directly connected via veth-provider.
func frrConnectedRoutesJSON(prefixes ...string) string {
	entries := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		entries = append(entries, fmt.Sprintf(
			`%q:[{"prefix":%q,"protocol":"connected","selected":true,"installed":true,`+
				`"nexthops":[{"directlyConnected":true,"interfaceName":"veth-provider"}]}]`, p, p))
	}
	return "{" + strings.Join(entries, ",") + "}"
}

// TestVethNexthopResolvable pins the detection side of #214: the next-hop counts
// as resolvable only when a *connected* prefix in the VRF covers it. The bug it
// exists to catch is a zebra that holds veth-provider in the VRF — and resolves
// the interface for other route types — while never having installed the /30's
// own connected prefix.
func TestVethNexthopResolvable(t *testing.T) {
	const query = "vtysh -c show ip route vrf vrf-provider connected json"

	tests := []struct {
		name    string
		output  string
		err     error
		want    bool
		wantErr bool
	}{
		{
			name:   "connected prefix covers the next-hop",
			output: frrConnectedRoutesJSON("169.254.0.0/30", "100.64.1.0/30"),
			want:   true,
		},
		{
			// The exact broken state from the nightly: the VRF's only
			// connected route is the uplink, and the veth /30 is absent.
			name:   "veth connected prefix missing",
			output: frrConnectedRoutesJSON("100.64.1.0/30"),
			want:   false,
		},
		{
			// FRR answers with an empty body, not "{}", when nothing matches.
			name:   "no connected routes at all",
			output: "",
			want:   false,
		},
		{
			name:   "empty json object",
			output: "{}",
			want:   false,
		},
		{
			name:    "vtysh failure is reported, not read as unresolvable",
			output:  "some vtysh error",
			err:     errors.New("exit status 1"),
			wantErr: true,
		},
		{
			name:    "malformed json is reported, not read as unresolvable",
			output:  "not json at all",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newVtyshRecorder()
			rec.on(strings.Fields(query), tt.output, tt.err)
			rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}}
			rm.execVtyshHook = rec.hook()

			got, err := rm.VethNexthopResolvable()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("VethNexthopResolvable() error = nil, want an error")
				}
				// An unreadable answer must not be mistaken for a
				// confirmed-unresolvable next-hop: the caller flaps an
				// address on that verdict.
				if got {
					t.Errorf("VethNexthopResolvable() = true on error, want false")
				}
				return
			}
			if err != nil {
				t.Fatalf("VethNexthopResolvable() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("VethNexthopResolvable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestVethNexthopResolvableDryRun proves the check never reports a repairable
// fault in dry-run, where there is no state to read and nothing to repair.
func TestVethNexthopResolvableDryRun(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1", DryRun: true}}
	rm.execVtyshHook = rec.hook()

	got, err := rm.VethNexthopResolvable()
	if err != nil || !got {
		t.Errorf("VethNexthopResolvable() in dry-run = (%v, %v), want (true, nil)", got, err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("dry-run issued %d vtysh calls, want 0", len(rec.calls))
	}
}

func TestRefreshVethNexthopDryRunAndDisabled(t *testing.T) {
	base := Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1", VethProviderIP: "169.254.0.2", VethLeakTableID: 200}

	dryRun := base
	dryRun.VethLeakEnabled = true
	dryRun.DryRun = true
	rm := &RouteManager{cfg: dryRun}
	if err := rm.RefreshVethNexthop(nil); err != nil {
		t.Errorf("RefreshVethNexthop() in dry-run should not error, got: %v", err)
	}

	disabled := base
	disabled.VethLeakEnabled = false
	rm = &RouteManager{cfg: disabled}
	if err := rm.RefreshVethNexthop(nil); err != nil {
		t.Errorf("RefreshVethNexthop() when disabled should not error, got: %v", err)
	}
}

func TestNewRouteManagerFRRPrefixList(t *testing.T) {
	cfg := Config{
		BridgeDev:     "br-ex",
		VRFName:       "vrf-provider",
		VethNexthop:   "169.254.0.1",
		FRRPrefixList: "ANNOUNCED-NETWORKS",
	}
	rm := NewRouteManager(cfg)
	if rm.cfg.FRRPrefixList != "ANNOUNCED-NETWORKS" {
		t.Errorf("frrPrefixList = %q, want %q", rm.cfg.FRRPrefixList, "ANNOUNCED-NETWORKS")
	}
}

func TestReconcileFRRPrefixListDisabled(t *testing.T) {
	rm := &RouteManager{cfg: Config{FRRPrefixList: ""}}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{cidr}, nil); err != nil {
		t.Errorf("ReconcileFRRPrefixList() with empty name should be no-op, got: %v", err)
	}
}

func TestReconcileFRRPrefixListDryRun(t *testing.T) {
	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS", DryRun: true}}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{cidr}, []string{"192.0.2.80"}); err != nil {
		t.Errorf("ReconcileFRRPrefixList() in dry-run should not error, got: %v", err)
	}
}

func TestReconcileVethLeakNetworksDisabled(t *testing.T) {
	rm := &RouteManager{cfg: Config{VethLeakEnabled: false}}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileVethLeakNetworks([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileVethLeakNetworks() when disabled should be no-op, got: %v", err)
	}
}

func TestReconcileVethLeakNetworksDryRun(t *testing.T) {
	rm := &RouteManager{cfg: Config{VethLeakEnabled: true, DryRun: true}}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileVethLeakNetworks([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileVethLeakNetworks() in dry-run should not error, got: %v", err)
	}
}

func TestNewRouteManagerPortForward(t *testing.T) {
	cfg := Config{
		BridgeDev:          "br-ex",
		VRFName:            "vrf-provider",
		VethNexthop:        "169.254.0.1",
		PortForwardEnabled: true,
		PortForwardDev:     "loopback1",
		PortForwardTableID: 202,
		PortForwards: []PortForwardVIP{
			{
				VIP:       "198.51.100.10",
				ManageVIP: true,
				Rules: []PortForwardRule{
					{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
				},
			},
		},
	}
	rm := NewRouteManager(cfg)

	if !rm.cfg.PortForwardEnabled {
		t.Error("portForwardEnabled should be true")
	}
	if rm.cfg.PortForwardDev != "loopback1" {
		t.Errorf("portForwardDev = %q, want %q", rm.cfg.PortForwardDev, "loopback1")
	}
	if rm.cfg.PortForwardTableID != 202 {
		t.Errorf("portForwardTableID = %d, want %d", rm.cfg.PortForwardTableID, 202)
	}
	if len(rm.cfg.PortForwards) != 1 {
		t.Errorf("len(portForwards) = %d, want 1", len(rm.cfg.PortForwards))
	}
	if rm.cfg.PortForwardL3mdevAccept {
		t.Error("portForwardL3mdevAccept should default to false")
	}
}

func TestNewRouteManagerPortForwardL3mdevAccept(t *testing.T) {
	cfg := Config{
		BridgeDev:               "br-ex",
		VRFName:                 "vrf-provider",
		VethNexthop:             "169.254.0.1",
		PortForwardEnabled:      true,
		PortForwardDev:          "loopback1",
		PortForwardTableID:      202,
		PortForwardL3mdevAccept: true,
		PortForwards: []PortForwardVIP{
			{
				VIP:       "198.51.100.10",
				ManageVIP: true,
				Rules: []PortForwardRule{
					{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"},
				},
			},
		},
	}
	rm := NewRouteManager(cfg)

	if !rm.cfg.PortForwardL3mdevAccept {
		t.Error("portForwardL3mdevAccept should be true when explicitly set")
	}
}

func TestDryRunPortForward(t *testing.T) {
	cfg := Config{
		BridgeDev:          "br-ex",
		VRFName:            "vrf-provider",
		VethNexthop:        "169.254.0.1",
		DryRun:             true,
		PortForwardEnabled: true,
		PortForwardDev:     "loopback1",
		PortForwardTableID: 201,
		PortForwards: []PortForwardVIP{
			{VIP: "198.51.100.10", Rules: []PortForwardRule{{Proto: "tcp", Port: 80, DestAddr: "10.0.0.100"}}},
		},
	}
	rm := NewRouteManager(cfg)

	if err := rm.SetupPortForward(); err != nil {
		t.Errorf("SetupPortForward() dry-run error: %v", err)
	}

	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	if err := rm.ReconcilePortForward([]*net.IPNet{cidr}, nil, true); err != nil {
		t.Errorf("ReconcilePortForward() dry-run error: %v", err)
	}

	if err := rm.TeardownPortForward(); err != nil {
		t.Errorf("TeardownPortForward() dry-run error: %v", err)
	}
}

func TestDisabledPortForward(t *testing.T) {
	cfg := Config{
		BridgeDev:          "br-ex",
		VRFName:            "vrf-provider",
		VethNexthop:        "169.254.0.1",
		PortForwardEnabled: false,
	}
	rm := NewRouteManager(cfg)

	if err := rm.SetupPortForward(); err != nil {
		t.Errorf("SetupPortForward() disabled error: %v", err)
	}
	if err := rm.ReconcilePortForward(nil, nil, true); err != nil {
		t.Errorf("ReconcilePortForward() disabled error: %v", err)
	}
	if err := rm.TeardownPortForward(); err != nil {
		t.Errorf("TeardownPortForward() disabled error: %v", err)
	}
}

// TestIsNoSuchRule mirrors TestIsNoSuchRoute for the rule path, where the
// kernel reports a missing rule as ENOENT.
func TestIsNoSuchRule(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bare ENOENT", syscall.ENOENT, true},
		{"wrapped ENOENT", fmt.Errorf("netlink: del rule: %w", syscall.ENOENT), true},
		{"a different errno", syscall.EPERM, false},
		{"ESRCH is a missing route, not a missing rule", syscall.ESRCH, false},
		{"text-only lookalike", errors.New("no such file or directory"), false},
		{"unrelated error", errors.New("permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNoSuchRule(tt.err); got != tt.want {
				t.Errorf("isNoSuchRule(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestHasFRRRoute_InvalidIPReturnsFalse(t *testing.T) {
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}}
	// Invalid IP must short-circuit before any vtysh exec.
	if rm.HasFRRRoute("not-an-ip") {
		t.Error("HasFRRRoute(invalid IP) should return false")
	}
	if rm.HasFRRRoute("") {
		t.Error("HasFRRRoute(empty) should return false")
	}
	if rm.HasFRRRoute("10.0.0.1/32") {
		t.Error("HasFRRRoute(CIDR notation) should return false")
	}
}

func TestListFRRPrefixListEntries_DisabledReturnsNil(t *testing.T) {
	// frrPrefixList empty → function returns (nil, nil) before any exec.
	rm := &RouteManager{cfg: Config{FRRPrefixList: ""}}
	entries, err := rm.ListFRRPrefixListEntries()
	if err != nil {
		t.Fatalf("expected nil error when prefix-list is disabled, got %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries when prefix-list is disabled, got %v", entries)
	}
}

func TestValidateIP(t *testing.T) {
	tests := []struct {
		ip      string
		wantErr bool
	}{
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"255.255.255.255", false},
		{"::1", true},         // IPv6 is rejected: the route paths are v4-only.
		{"2001:db8::1", true}, // IPv6 is rejected.
		{"", true},
		{"not-an-ip", true},
		{"10.0.0.1/32", true},
		{"10.0.0", true},
	}

	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			err := validateIP(tt.ip)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateIP(%q) error = %v, wantErr %v", tt.ip, err, tt.wantErr)
			}
		})
	}
}

// =============================================================================
// vtysh-hook-based tests for the FRR helpers in routing.go
// =============================================================================

func TestHasFRRRoute_ParsesVtyshOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
		want   bool
	}{
		{
			"static route present",
			"Routing entry for 198.51.100.10/32\n  Known via \"static\", distance 1, metric 0\n  veth-default\n",
			nil, true,
		},
		{
			"route absent — no 'static' substring",
			"Routing entry for 198.51.100.10/32\n  Known via \"connected\", distance 0, metric 0\n",
			nil, false,
		},
		{"empty output", "", nil, false},
		{"vtysh exec error returns false", "boom", errors.New("vtysh failed"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newVtyshRecorder()
			rec.on(
				[]string{"vtysh", "-c", "show ip route vrf vrf-provider 198.51.100.10/32"},
				tt.output, tt.err,
			)
			rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
			if got := rm.HasFRRRoute("198.51.100.10"); got != tt.want {
				t.Errorf("HasFRRRoute() = %v, want %v", got, tt.want)
			}
			if len(rec.calls) != 1 {
				t.Errorf("expected 1 vtysh call, got %d: %v", len(rec.calls), rec.calls)
			}
		})
	}
}

// TestListFRRRoutes_ParsesStaticRoutes pins the nexthop-scoped ownership rule
// against the JSON document: the agent owns only the statics resolved via its
// own veth nexthop. An operator static via a foreign nexthop must be excluded —
// treating it as agent-owned would withdraw it on the next reconcile. A
// non-/32 static is likewise not an agent route.
func TestListFRRRoutes_ParsesStaticRoutes(t *testing.T) {
	const doc = `{
	  "198.51.100.10/32": [{"prefix":"198.51.100.10/32","protocol":"static","selected":true,"installed":true,
	    "nexthops":[{"ip":"169.254.0.1","active":true}]}],
	  "198.51.100.11/32": [{"prefix":"198.51.100.11/32","protocol":"static","selected":true,"installed":true,
	    "nexthops":[{"ip":"169.254.0.1","active":true}]}],
	  "192.0.2.99/32": [{"prefix":"192.0.2.99/32","protocol":"static","selected":true,"installed":true,
	    "nexthops":[{"ip":"192.0.2.1","active":true}]}],
	  "203.0.113.0/24": [{"prefix":"203.0.113.0/24","protocol":"static","selected":true,"installed":true,
	    "nexthops":[{"ip":"169.254.0.1","active":true}]}]
	}`
	rec := newVtyshRecorder()
	rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"}, doc, nil)
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}

	got, err := rm.ListFRRRoutes()
	if err != nil {
		t.Fatalf("ListFRRRoutes: %v", err)
	}
	want := []string{"198.51.100.10", "198.51.100.11"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListFRRRoutes() = %v, want %v (foreign nexthop and non-/32 must be excluded)", got, want)
	}
}

// TestListFRRRoutes_EmptyVRF covers FRR's empty-body answer for a VRF with no
// static routes — it must read as "nothing configured", not as an error.
func TestListFRRRoutes_EmptyVRF(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"}, "", nil)
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}

	got, err := rm.ListFRRRoutes()
	if err != nil {
		t.Fatalf("ListFRRRoutes on an empty VRF: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListFRRRoutes() = %v, want empty", got)
	}
}

// TestListFRRRoutes_MalformedJSON must surface a parse failure rather than
// silently reporting "no routes" — an empty list would withdraw every FIP.
func TestListFRRRoutes_MalformedJSON(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"}, "{not json", nil)
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}

	if _, err := rm.ListFRRRoutes(); err == nil {
		t.Fatal("expected a parse error for malformed JSON, got nil (an empty list would withdraw every FIP)")
	}
}

func TestListFRRRoutes_PropagatesVtyshError(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"},
		"", errors.New("exit 1"),
	)
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
	if _, err := rm.ListFRRRoutes(); err == nil {
		t.Fatal("expected error from ListFRRRoutes when vtysh fails, got nil")
	}
}

func TestInactiveFRRRoutes(t *testing.T) {
	const cmd = "show ip route vrf vrf-provider static json"

	t.Run("all selected and installed are active", func(t *testing.T) {
		j := `{
  "198.51.100.10/32": [{"prefix":"198.51.100.10/32","protocol":"static","selected":true,"installed":true}],
  "198.51.100.11/32": [{"prefix":"198.51.100.11/32","protocol":"static","selected":true,"installed":true}]
}`
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", cmd}, j, nil)
		rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
		got, err := rm.InactiveFRRRoutes([]string{"198.51.100.10", "198.51.100.11"})
		if err != nil {
			t.Fatalf("InactiveFRRRoutes: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want no inactive routes", got)
		}
	})

	t.Run("reports configured-but-inactive, ignores absent", func(t *testing.T) {
		// .10 active; .11 configured but neither selected nor installed;
		// .12 not configured at all (absent → not this check's concern).
		j := `{
  "198.51.100.10/32": [{"prefix":"198.51.100.10/32","protocol":"static","selected":true,"installed":true}],
  "198.51.100.11/32": [{"prefix":"198.51.100.11/32","protocol":"static"}]
}`
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", cmd}, j, nil)
		rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
		got, err := rm.InactiveFRRRoutes([]string{"198.51.100.10", "198.51.100.11", "198.51.100.12"})
		if err != nil {
			t.Fatalf("InactiveFRRRoutes: %v", err)
		}
		if !reflect.DeepEqual(got, []string{"198.51.100.11"}) {
			t.Errorf("got %v, want [198.51.100.11]", got)
		}
	})

	t.Run("selected but not installed is inactive", func(t *testing.T) {
		j := `{"198.51.100.11/32":[{"prefix":"198.51.100.11/32","protocol":"static","selected":true}]}`
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", cmd}, j, nil)
		rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
		got, _ := rm.InactiveFRRRoutes([]string{"198.51.100.11"})
		if !reflect.DeepEqual(got, []string{"198.51.100.11"}) {
			t.Errorf("got %v, want [198.51.100.11]", got)
		}
	})

	t.Run("empty body means nothing configured", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", cmd}, "", nil)
		rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
		got, err := rm.InactiveFRRRoutes([]string{"198.51.100.10"})
		if err != nil || got != nil {
			t.Errorf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("vtysh error propagates", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", cmd}, "boom", errors.New("exit 1"))
		rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
		if _, err := rm.InactiveFRRRoutes([]string{"198.51.100.10"}); err == nil {
			t.Fatal("expected error when vtysh fails")
		}
	})

	t.Run("dry-run and empty input short-circuit", func(t *testing.T) {
		rmDry := &RouteManager{cfg: Config{VRFName: "vrf-provider", DryRun: true}}
		if got, err := rmDry.InactiveFRRRoutes([]string{"198.51.100.10"}); got != nil || err != nil {
			t.Errorf("dry-run: got (%v, %v), want (nil, nil)", got, err)
		}
		rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}}
		if got, err := rm.InactiveFRRRoutes(nil); got != nil || err != nil {
			t.Errorf("empty input: got (%v, %v), want (nil, nil)", got, err)
		}
	})
}

// TestStaticFRRRoutesMemoizedPerCycle proves the shared static-route document
// is read from vtysh at most once per reconcile cycle. ListFRRRoutes and
// InactiveFRRRoutes now issue the identical `show ip route ... static json`
// query, so without the per-cycle memo a no-change cycle would fork vtysh twice
// for the same document. resetFRRRouteCache (start of each cycle) and a route
// mutation must each force a fresh read.
func TestStaticFRRRoutesMemoizedPerCycle(t *testing.T) {
	const query = "vtysh -c show ip route vrf vrf-provider static json"
	doc := frrStaticRoutesJSON("169.254.0.1", "198.51.100.10")
	rec := newVtyshRecorder()
	rec.on([]string{"vtysh", "-c", "show ip route vrf vrf-provider static json"}, doc, nil)
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}

	countQuery := func() int {
		n := 0
		for _, c := range rec.calls {
			if strings.Join(c, " ") == query {
				n++
			}
		}
		return n
	}

	// Two readers in one cycle share a single vtysh read.
	if _, err := rm.ListFRRRoutes(); err != nil {
		t.Fatalf("ListFRRRoutes: %v", err)
	}
	if _, err := rm.InactiveFRRRoutes([]string{"198.51.100.10"}); err != nil {
		t.Fatalf("InactiveFRRRoutes: %v", err)
	}
	if got := countQuery(); got != 1 {
		t.Fatalf("static-route query ran %d times in one cycle, want 1 (readers must share the memo)", got)
	}

	// A new cycle re-reads.
	rm.resetFRRRouteCache()
	if _, err := rm.ListFRRRoutes(); err != nil {
		t.Fatalf("ListFRRRoutes after reset: %v", err)
	}
	if got := countQuery(); got != 2 {
		t.Fatalf("static-route query ran %d times, want 2 (resetFRRRouteCache must force a fresh read)", got)
	}

	// A route mutation invalidates the memo, so the next reader re-reads.
	if err := rm.AddFRRRoutes([]string{"198.51.100.11"}); err != nil {
		t.Fatalf("AddFRRRoutes: %v", err)
	}
	if _, err := rm.InactiveFRRRoutes([]string{"198.51.100.10"}); err != nil {
		t.Fatalf("InactiveFRRRoutes after add: %v", err)
	}
	if got := countQuery(); got != 3 {
		t.Fatalf("static-route query ran %d times, want 3 (a route mutation must invalidate the memo)", got)
	}
}

func TestAddFRRRoutesBatchesVtyshCommands(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
	if err := rm.AddFRRRoutes([]string{"10.0.0.1", "10.0.0.2"}); err != nil {
		t.Fatalf("AddFRRRoutes: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected exactly one vtysh batch, got %d: %v", len(rec.calls), rec.calls)
	}
	joined := strings.Join(rec.calls[0], " ")
	if !strings.Contains(joined, "ip route 10.0.0.1/32 169.254.0.1") {
		t.Errorf("first IP missing from batch: %v", rec.calls[0])
	}
	if !strings.Contains(joined, "ip route 10.0.0.2/32 169.254.0.1") {
		t.Errorf("second IP missing from batch: %v", rec.calls[0])
	}
	if !strings.Contains(joined, "vrf vrf-provider") {
		t.Errorf("vrf header missing from batch: %v", rec.calls[0])
	}
}

func TestAddFRRRoutes_PropagatesVtyshError(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
	// Override the hook with one that always errors.
	rm.execVtyshHook = func(cmd *exec.Cmd) ([]byte, error) {
		rec.calls = append(rec.calls, append([]string{}, cmd.Args...))
		return []byte("err"), errors.New("vtysh failed")
	}
	if err := rm.AddFRRRoutes([]string{"10.0.0.1"}); err == nil {
		t.Fatal("expected error when vtysh exec fails, got nil")
	}
}

func TestDelFRRRoutesBatchesVtyshCommands(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: rec.hook()}
	if err := rm.DelFRRRoutes([]string{"10.0.0.1", "10.0.0.2"}); err != nil {
		t.Fatalf("DelFRRRoutes: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected one batched vtysh call, got %d", len(rec.calls))
	}
	joined := strings.Join(rec.calls[0], " ")
	if !strings.Contains(joined, "no ip route 10.0.0.1/32 169.254.0.1") {
		t.Errorf("expected 'no ip route' for 10.0.0.1, got: %s", joined)
	}
	if !strings.Contains(joined, "no ip route 10.0.0.2/32 169.254.0.1") {
		t.Errorf("expected 'no ip route' for 10.0.0.2, got: %s", joined)
	}
}

func TestDelFRRRoutes_PropagatesVtyshError(t *testing.T) {
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider", VethNexthop: "169.254.0.1"}, execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
		return []byte("err"), errors.New("vtysh failed")
	}}
	if err := rm.DelFRRRoutes([]string{"10.0.0.1"}); err == nil {
		t.Fatal("expected error when vtysh exec fails, got nil")
	}
}

func TestRefreshBGPInvokesVtysh(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: rec.hook()}
	if err := rm.RefreshBGP(); err != nil {
		t.Fatalf("RefreshBGP: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("expected one vtysh call, got %d", len(rec.calls))
	}
	joined := strings.Join(rec.calls[0], " ")
	if !strings.Contains(joined, "clear ip bgp vrf vrf-provider * soft out") {
		t.Errorf("RefreshBGP did not issue the BGP soft-refresh command: %s", joined)
	}
}

func TestRefreshBGP_PropagatesVtyshError(t *testing.T) {
	rm := &RouteManager{cfg: Config{VRFName: "vrf-provider"}, execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
		return []byte("err"), errors.New("vtysh failed")
	}}
	if err := rm.RefreshBGP(); err == nil {
		t.Fatal("expected error when BGP soft-refresh fails, got nil")
	}
}

func TestListFRRPrefixListEntries_Parses(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []prefixListEntry
	}{
		{
			"valid entries",
			frrPrefixListJSON("ANNOUNCED-NETWORKS",
				prefixListEntry{Seq: 5, Network: "198.51.100.0/24"},
				prefixListEntry{Seq: 10, Network: "203.0.113.0/24"},
			),
			[]prefixListEntry{
				{Seq: 5, Network: "198.51.100.0/24"},
				{Seq: 10, Network: "203.0.113.0/24"},
			},
		},
		{
			// Only the agent-managed shape (permit … ge 32 le 32) is ours: a
			// deny entry, or a permit without the /32 range, belongs to the
			// operator and must never be reported (and so never removed).
			"non-managed entries are skipped",
			`{"ZEBRA":{"ANNOUNCED-NETWORKS":{"addressFamily":"IPv4","entries":[
			  {"sequenceNumber":5,"type":"permit","prefix":"198.51.100.0/24"},
			  {"sequenceNumber":10,"type":"deny","prefix":"10.0.0.0/8","minimumPrefixLength":32,"maximumPrefixLength":32},
			  {"sequenceNumber":15,"type":"permit","prefix":"172.16.0.0/12","minimumPrefixLength":24,"maximumPrefixLength":32}
			]}}}`,
			nil,
		},
		{"empty output", "", nil},
		// A prefix-list that does not exist yet: FRR answers with an empty
		// JSON document rather than the old "Can't find" prose.
		{"missing list returns nil", "{}", nil},
		{
			// The document exists but describes a different list — ours is
			// still absent.
			"other list only returns nil",
			frrPrefixListJSON("SOME-OTHER-LIST", prefixListEntry{Seq: 5, Network: "198.51.100.0/24"}),
			nil,
		},
		{"list with no entries", `{"ZEBRA":{"ANNOUNCED-NETWORKS":{"addressFamily":"IPv4","entries":[]}}}`, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newVtyshRecorder()
			rec.on(
				[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS json"},
				tt.output, nil,
			)
			rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: rec.hook()}
			got, err := rm.ListFRRPrefixListEntries()
			if err != nil {
				t.Fatalf("ListFRRPrefixListEntries: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("entries = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestListFRRPrefixListEntries_MultiDaemonConcatenation covers the real vtysh
// output: each answering daemon nests the list under its own protocol name and
// vtysh concatenates the documents, so the body holds more than one top-level
// object ({"ZEBRA":{…}}{"BGP":{…}}). A single json.Unmarshal rejects the second
// with "invalid character '{' after top-level value" — the failure the
// integration and E2E suites hit — and the entries live one level below the
// daemon key, not at the top.
func TestListFRRPrefixListEntries_MultiDaemonConcatenation(t *testing.T) {
	e5 := prefixListEntry{Seq: 5, Network: "198.51.100.0/24"}
	e10 := prefixListEntry{Seq: 10, Network: "203.0.113.0/24"}
	want := []prefixListEntry{e5, e10}
	tests := []struct {
		name   string
		output string
		want   []prefixListEntry
	}{
		// zebra and bgpd both report the list with the same entries; the
		// duplicate daemon copy must be deduplicated, not doubled.
		{
			"zebra and bgp agree",
			frrPrefixListDoc("ZEBRA", "ANNOUNCED-NETWORKS", e5, e10) +
				frrPrefixListDoc("BGP", "ANNOUNCED-NETWORKS", e5, e10),
			want,
		},
		// Only one daemon carries the entries; the other reports the list empty.
		{
			"one daemon empty",
			frrPrefixListDoc("ZEBRA", "ANNOUNCED-NETWORKS", e5, e10) +
				frrPrefixListDoc("BGP", "ANNOUNCED-NETWORKS"),
			want,
		},
		// The empty daemon document arrives first.
		{
			"empty daemon first",
			frrPrefixListDoc("BGP", "ANNOUNCED-NETWORKS") +
				frrPrefixListDoc("ZEBRA", "ANNOUNCED-NETWORKS", e5, e10),
			want,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newVtyshRecorder()
			rec.on(
				[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS json"},
				tt.output, nil,
			)
			rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: rec.hook()}
			got, err := rm.ListFRRPrefixListEntries()
			if err != nil {
				t.Fatalf("ListFRRPrefixListEntries: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("entries = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestListFRRPrefixListEntries_MalformedJSON must surface the parse failure:
// reporting "no entries" would make ReconcileFRRPrefixList re-add every network
// on every cycle.
func TestListFRRPrefixListEntries_MalformedJSON(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS json"},
		"{not json", nil,
	)
	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: rec.hook()}
	if _, err := rm.ListFRRPrefixListEntries(); err == nil {
		t.Fatal("expected a parse error for malformed JSON, got nil")
	}
}

func TestListFRRPrefixListEntries_PropagatesVtyshError(t *testing.T) {
	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
		return []byte("err"), errors.New("vtysh failed")
	}}
	if _, err := rm.ListFRRPrefixListEntries(); err == nil {
		t.Fatal("expected error when vtysh fails, got nil")
	}
}

func TestReconcileFRRPrefixList_AddsMissingAndRemovesStale(t *testing.T) {
	rec := newVtyshRecorder()
	// Initial state has 10.0.0.0/24 (stale) and 198.51.100.0/24 (desired).
	rec.on(
		[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS json"},
		frrPrefixListJSON("ANNOUNCED-NETWORKS",
			prefixListEntry{Seq: 5, Network: "10.0.0.0/24"},
			prefixListEntry{Seq: 10, Network: "198.51.100.0/24"},
		),
		nil,
	)

	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: rec.hook()}

	_, desired1, _ := net.ParseCIDR("198.51.100.0/24")
	_, desired2, _ := net.ParseCIDR("203.0.113.0/24") // new
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{desired1, desired2}, nil); err != nil {
		t.Fatalf("ReconcileFRRPrefixList: %v", err)
	}

	// Expect: 1 list call + 1 add (203.0.113.0/24) + 1 remove (10.0.0.0/24).
	if len(rec.calls) != 3 {
		t.Fatalf("expected 3 vtysh calls (list+add+remove), got %d: %v", len(rec.calls), rec.calls)
	}

	var sawAdd, sawRemove bool
	for _, c := range rec.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "ip prefix-list ANNOUNCED-NETWORKS seq") &&
			strings.Contains(j, "permit 203.0.113.0/24") &&
			!strings.Contains(j, "no ip prefix-list") {
			sawAdd = true
		}
		if strings.Contains(j, "no ip prefix-list ANNOUNCED-NETWORKS seq 5") &&
			strings.Contains(j, "permit 10.0.0.0/24") {
			sawRemove = true
		}
	}
	if !sawAdd {
		t.Errorf("expected an 'add' call for 203.0.113.0/24, got: %v", rec.calls)
	}
	if !sawRemove {
		t.Errorf("expected a 'remove' call for 10.0.0.0/24, got: %v", rec.calls)
	}
}

// TestListFRRPrefixListEntries_ExactVIPShape pins the second managed entry
// shape (#226): an exact "permit <vip>/32" comes back from FRR with no
// minimumPrefixLength/maximumPrefixLength keys and must be parsed with
// Exact set, while a foreign exact entry that is not a /32 stays invisible
// to reconcile.
func TestListFRRPrefixListEntries_ExactVIPShape(t *testing.T) {
	vip := prefixListEntry{Seq: 20, Network: "192.0.2.80/32", Exact: true}
	network := prefixListEntry{Seq: 10, Network: "198.51.100.0/24"}
	foreign := prefixListEntry{Seq: 30, Network: "10.0.0.0/8", Exact: true}

	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS json"},
		frrPrefixListJSON("ANNOUNCED-NETWORKS", network, vip, foreign),
		nil,
	)
	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: rec.hook()}
	got, err := rm.ListFRRPrefixListEntries()
	if err != nil {
		t.Fatalf("ListFRRPrefixListEntries: %v", err)
	}
	want := []prefixListEntry{network, vip}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %+v, want %+v", got, want)
	}
}

// TestReconcileFRRPrefixList_VIPEntries pins the announceable-VIP half of the
// reconcile (#226): a missing VIP gets an exact "permit <vip>/32" entry with
// no ge/le qualifier, a stale VIP entry is removed with the exact form it was
// created with, and a present VIP entry is left alone.
func TestReconcileFRRPrefixList_VIPEntries(t *testing.T) {
	rec := newVtyshRecorder()
	// Initial state: the desired network, a kept VIP, and a stale VIP.
	rec.on(
		[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS json"},
		frrPrefixListJSON("ANNOUNCED-NETWORKS",
			prefixListEntry{Seq: 10, Network: "198.51.100.0/24"},
			prefixListEntry{Seq: 15, Network: "192.0.2.80/32", Exact: true},
			prefixListEntry{Seq: 20, Network: "192.0.2.99/32", Exact: true},
		),
		nil,
	)

	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: rec.hook()}

	_, network, _ := net.ParseCIDR("198.51.100.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{network}, []string{"192.0.2.80", "192.0.2.81"}); err != nil {
		t.Fatalf("ReconcileFRRPrefixList: %v", err)
	}

	// Expect: 1 list call + 1 add (192.0.2.81/32) + 1 remove (192.0.2.99/32).
	if len(rec.calls) != 3 {
		t.Fatalf("expected 3 vtysh calls (list+add+remove), got %d: %v", len(rec.calls), rec.calls)
	}

	var sawAdd, sawRemove bool
	for _, c := range rec.calls {
		j := strings.Join(c, " ")
		if strings.HasSuffix(j, "permit 192.0.2.81/32 -c end") &&
			strings.Contains(j, "ip prefix-list ANNOUNCED-NETWORKS seq") &&
			!strings.Contains(j, "no ip prefix-list") {
			sawAdd = true
		}
		if strings.HasSuffix(j, "permit 192.0.2.99/32 -c end") &&
			strings.Contains(j, "no ip prefix-list ANNOUNCED-NETWORKS seq 20") {
			sawRemove = true
		}
	}
	if !sawAdd {
		t.Errorf("expected an exact-form add for 192.0.2.81/32, got: %v", rec.calls)
	}
	if !sawRemove {
		t.Errorf("expected an exact-form remove for 192.0.2.99/32, got: %v", rec.calls)
	}
}

// TestReconcileFRRPrefixList_ShapeMismatchConverges pins the shape keying: the
// same prefix string in the wrong shape (a leftover "permit <vip>/32 ge 32 le
// 32") does not satisfy the desired exact entry — reconcile adds the exact
// form and removes the ge/le form, regenerating each config line in the shape
// it was created with.
func TestReconcileFRRPrefixList_ShapeMismatchConverges(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS json"},
		frrPrefixListJSON("ANNOUNCED-NETWORKS",
			prefixListEntry{Seq: 5, Network: "192.0.2.80/32"}, // ge/le shape
		),
		nil,
	)

	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: rec.hook()}
	if err := rm.ReconcileFRRPrefixList(nil, []string{"192.0.2.80"}); err != nil {
		t.Fatalf("ReconcileFRRPrefixList: %v", err)
	}

	var sawExactAdd, sawGeLeRemove bool
	for _, c := range rec.calls {
		j := strings.Join(c, " ")
		if strings.HasSuffix(j, "permit 192.0.2.80/32 -c end") && !strings.Contains(j, "no ip prefix-list") {
			sawExactAdd = true
		}
		if strings.Contains(j, "no ip prefix-list ANNOUNCED-NETWORKS seq 5 permit 192.0.2.80/32 ge 32 le 32") {
			sawGeLeRemove = true
		}
	}
	if !sawExactAdd {
		t.Errorf("expected an exact-form add for 192.0.2.80/32, got: %v", rec.calls)
	}
	if !sawGeLeRemove {
		t.Errorf("expected a ge/le-form remove for 192.0.2.80/32, got: %v", rec.calls)
	}
}

func TestReconcileFRRPrefixList_AddFailureBailsOut(t *testing.T) {
	calls := 0
	rm := &RouteManager{cfg: Config{FRRPrefixList: "ANNOUNCED-NETWORKS"}, execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
		calls++
		joined := strings.Join(cmd.Args, " ")
		if strings.Contains(joined, "show ip prefix-list") {
			return nil, nil
		}
		return []byte("error output"), errors.New("vtysh add failed")
	}}
	_, n, _ := net.ParseCIDR("198.51.100.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{n}, nil); err == nil {
		t.Fatal("expected error when add command fails, got nil")
	}
}

func TestRunVtyshUsesHookWhenSet(t *testing.T) {
	var captured []string
	rm := &RouteManager{
		execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
			captured = append([]string{}, cmd.Args...)
			return []byte("stub-output"), nil
		},
	}
	out, err := rm.runVtysh("-c", "show running-config")
	if err != nil {
		t.Fatalf("runVtysh: %v", err)
	}
	if string(out) != "stub-output" {
		t.Errorf("runVtysh output = %q, want %q", out, "stub-output")
	}
	want := []string{"vtysh", "-c", "show running-config"}
	if !reflect.DeepEqual(captured, want) {
		t.Errorf("hook captured %v, want %v", captured, want)
	}
}
