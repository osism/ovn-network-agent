package main

import (
	"errors"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

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

func TestIsNoSuchRoute(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no such process", errors.New("no such process"), true},
		{"wrapped no such process", errors.New("netlink: del route: no such process"), true},
		{"other error", errors.New("permission denied"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNoSuchRoute(tt.err)
			if got != tt.want {
				t.Errorf("isNoSuchRoute(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNewRouteManager(t *testing.T) {
	cfg := Config{
		BridgeDev:   "br-ex",
		VRFName:     "vrf-provider",
		VethNexthop: "169.254.0.1",
	}

	rm := NewRouteManager(cfg)

	if rm.bridgeDev != "br-ex" {
		t.Errorf("bridgeDev = %q, want %q", rm.bridgeDev, "br-ex")
	}
	if rm.vrfName != "vrf-provider" {
		t.Errorf("vrfName = %q, want %q", rm.vrfName, "vrf-provider")
	}
	if rm.vethNexthop != "169.254.0.1" {
		t.Errorf("vethNexthop = %q, want %q", rm.vethNexthop, "169.254.0.1")
	}
	if rm.routeTableID != 0 {
		t.Errorf("routeTableID = %d, want 0", rm.routeTableID)
	}
	if rm.dryRun {
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

	if rm.routeTableID != 100 {
		t.Errorf("routeTableID = %d, want 100", rm.routeTableID)
	}
}

func TestDryRunBridgeIP(t *testing.T) {
	rm := &RouteManager{
		bridgeDev: "br-ex",
		dryRun:    true,
	}

	if err := rm.EnsureBridgeIP("169.254.169.254"); err != nil {
		t.Errorf("EnsureBridgeIP() in dry-run should not error, got: %v", err)
	}
	if err := rm.RemoveBridgeIP("169.254.169.254"); err != nil {
		t.Errorf("RemoveBridgeIP() in dry-run should not error, got: %v", err)
	}
}

func TestDryRunOVSFlows(t *testing.T) {
	rm := &RouteManager{
		bridgeDev: "br-ex",
		dryRun:    true,
	}

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

	if !rm.dryRun {
		t.Error("dryRun should be true when config has DryRun=true")
	}
}

func TestDryRunFRRRoutes(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}

	if err := rm.AddFRRRoute("10.0.0.1"); err != nil {
		t.Errorf("AddFRRRoute() in dry-run should not error, got: %v", err)
	}
	if err := rm.DelFRRRoute("10.0.0.1"); err != nil {
		t.Errorf("DelFRRRoute() in dry-run should not error, got: %v", err)
	}
}

func TestDryRunFRRRoutesBatch(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}

	ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	if err := rm.AddFRRRoutes(ips); err != nil {
		t.Errorf("AddFRRRoutes() in dry-run should not error, got: %v", err)
	}
	if err := rm.DelFRRRoutes(ips); err != nil {
		t.Errorf("DelFRRRoutes() in dry-run should not error, got: %v", err)
	}
}

func TestFRRRoutesBatchEmpty(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
	}

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
	rm := &RouteManager{
		vrfName: "vrf-provider",
		dryRun:  true,
	}
	if err := rm.RefreshBGP(); err != nil {
		t.Errorf("RefreshBGP() in dry-run should not error, got: %v", err)
	}
}

func TestFRRRoutesBatchValidation(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:   "br-ex",
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		dryRun:      true,
	}

	invalid := []string{"10.0.0.1", "not-an-ip", "10.0.0.2"}
	if err := rm.AddFRRRoutes(invalid); err == nil {
		t.Error("AddFRRRoutes() with invalid IP should return error")
	}
	if err := rm.DelFRRRoutes(invalid); err == nil {
		t.Error("DelFRRRoutes() with invalid IP should return error")
	}

	// CIDR notation is not a valid bare IP.
	cidr := []string{"10.0.0.1/32"}
	if err := rm.AddFRRRoutes(cidr); err == nil {
		t.Error("AddFRRRoutes() with CIDR notation should return error")
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

	if !rm.vethLeakEnabled {
		t.Error("vethLeakEnabled should be true")
	}
	if rm.vethProviderIP != "169.254.0.2" {
		t.Errorf("vethProviderIP = %q, want %q", rm.vethProviderIP, "169.254.0.2")
	}
	if rm.vethLeakTableID != 200 {
		t.Errorf("vethLeakTableID = %d, want 200", rm.vethLeakTableID)
	}
	if rm.vethLeakRulePriority != 2000 {
		t.Errorf("vethLeakRulePriority = %d, want 2000", rm.vethLeakRulePriority)
	}
	if len(rm.networkFilters) != 1 {
		t.Errorf("networkFilters length = %d, want 1", len(rm.networkFilters))
	}
}

func TestDryRunVethLeak(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		vrfName:         "vrf-provider",
		vethNexthop:     "169.254.0.1",
		vethLeakEnabled: true,
		vethProviderIP:  "169.254.0.2",
		vethLeakTableID: 200,
		dryRun:          true,
	}

	if err := rm.SetupVethLeak(); err != nil {
		t.Errorf("SetupVethLeak() in dry-run should not error, got: %v", err)
	}
	if err := rm.TeardownVethLeak(); err != nil {
		t.Errorf("TeardownVethLeak() in dry-run should not error, got: %v", err)
	}
}

func TestDisabledVethLeak(t *testing.T) {
	rm := &RouteManager{
		bridgeDev:       "br-ex",
		vrfName:         "vrf-provider",
		vethNexthop:     "169.254.0.1",
		vethLeakEnabled: false,
	}

	if err := rm.SetupVethLeak(); err != nil {
		t.Errorf("SetupVethLeak() when disabled should not error, got: %v", err)
	}
	if err := rm.TeardownVethLeak(); err != nil {
		t.Errorf("TeardownVethLeak() when disabled should not error, got: %v", err)
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
	if rm.frrPrefixList != "ANNOUNCED-NETWORKS" {
		t.Errorf("frrPrefixList = %q, want %q", rm.frrPrefixList, "ANNOUNCED-NETWORKS")
	}
}

func TestReconcileFRRPrefixListDisabled(t *testing.T) {
	rm := &RouteManager{frrPrefixList: ""}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileFRRPrefixList() with empty name should be no-op, got: %v", err)
	}
}

func TestReconcileFRRPrefixListDryRun(t *testing.T) {
	rm := &RouteManager{frrPrefixList: "ANNOUNCED-NETWORKS", dryRun: true}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileFRRPrefixList() in dry-run should not error, got: %v", err)
	}
}

func TestReconcileVethLeakNetworksDisabled(t *testing.T) {
	rm := &RouteManager{vethLeakEnabled: false}
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	if err := rm.ReconcileVethLeakNetworks([]*net.IPNet{cidr}); err != nil {
		t.Errorf("ReconcileVethLeakNetworks() when disabled should be no-op, got: %v", err)
	}
}

func TestReconcileVethLeakNetworksDryRun(t *testing.T) {
	rm := &RouteManager{vethLeakEnabled: true, dryRun: true}
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

	if !rm.portForwardEnabled {
		t.Error("portForwardEnabled should be true")
	}
	if rm.portForwardDev != "loopback1" {
		t.Errorf("portForwardDev = %q, want %q", rm.portForwardDev, "loopback1")
	}
	if rm.portForwardTableID != 202 {
		t.Errorf("portForwardTableID = %d, want %d", rm.portForwardTableID, 202)
	}
	if len(rm.portForwards) != 1 {
		t.Errorf("len(portForwards) = %d, want 1", len(rm.portForwards))
	}
	if rm.portForwardL3mdevAccept {
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

	if !rm.portForwardL3mdevAccept {
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
	if err := rm.ReconcilePortForward([]*net.IPNet{cidr}, nil); err != nil {
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
	if err := rm.ReconcilePortForward(nil, nil); err != nil {
		t.Errorf("ReconcilePortForward() disabled error: %v", err)
	}
	if err := rm.TeardownPortForward(); err != nil {
		t.Errorf("TeardownPortForward() disabled error: %v", err)
	}
}

func TestIsNoSuchRule(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no such file or directory", errors.New("no such file or directory"), true},
		{"wrapped no such file", errors.New("netlink: del rule: no such file or directory"), true},
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
	rm := &RouteManager{vrfName: "vrf-provider"}
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
	rm := &RouteManager{frrPrefixList: ""}
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
			rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
			if got := rm.HasFRRRoute("198.51.100.10"); got != tt.want {
				t.Errorf("HasFRRRoute() = %v, want %v", got, tt.want)
			}
			if len(rec.calls) != 1 {
				t.Errorf("expected 1 vtysh call, got %d: %v", len(rec.calls), rec.calls)
			}
		})
	}
}

func TestListFRRRoutes_ParsesStaticRoutes(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		`S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
S>* 198.51.100.11/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
C>* 10.0.0.0/24 is directly connected, br-ex, 00:00:01
S>* 203.0.113.10/32 [1/0] via 169.254.0.1, veth-default, weight 1, 00:00:01
`,
		nil,
	)
	rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}

	got, err := rm.ListFRRRoutes()
	if err != nil {
		t.Fatalf("ListFRRRoutes: %v", err)
	}
	want := []string{"198.51.100.10", "198.51.100.11", "203.0.113.10"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListFRRRoutes() = %v, want %v", got, want)
	}
}

func TestListFRRRoutes_PropagatesVtyshError(t *testing.T) {
	rec := newVtyshRecorder()
	rec.on(
		[]string{"vtysh", "-c", "show ip route vrf vrf-provider static"},
		"connection refused", errors.New("exit 1"),
	)
	rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
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
		rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
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
		rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
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
		rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
		got, _ := rm.InactiveFRRRoutes([]string{"198.51.100.11"})
		if !reflect.DeepEqual(got, []string{"198.51.100.11"}) {
			t.Errorf("got %v, want [198.51.100.11]", got)
		}
	})

	t.Run("empty body means nothing configured", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", cmd}, "", nil)
		rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
		got, err := rm.InactiveFRRRoutes([]string{"198.51.100.10"})
		if err != nil || got != nil {
			t.Errorf("got (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("vtysh error propagates", func(t *testing.T) {
		rec := newVtyshRecorder()
		rec.on([]string{"vtysh", "-c", cmd}, "boom", errors.New("exit 1"))
		rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
		if _, err := rm.InactiveFRRRoutes([]string{"198.51.100.10"}); err == nil {
			t.Fatal("expected error when vtysh fails")
		}
	})

	t.Run("dry-run and empty input short-circuit", func(t *testing.T) {
		rmDry := &RouteManager{vrfName: "vrf-provider", dryRun: true}
		if got, err := rmDry.InactiveFRRRoutes([]string{"198.51.100.10"}); got != nil || err != nil {
			t.Errorf("dry-run: got (%v, %v), want (nil, nil)", got, err)
		}
		rm := &RouteManager{vrfName: "vrf-provider"}
		if got, err := rm.InactiveFRRRoutes(nil); got != nil || err != nil {
			t.Errorf("empty input: got (%v, %v), want (nil, nil)", got, err)
		}
	})
}

func TestAddFRRRoutesBatchesVtyshCommands(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
	}
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
	rm := &RouteManager{
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
	}
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
	rm := &RouteManager{
		vrfName:       "vrf-provider",
		vethNexthop:   "169.254.0.1",
		execVtyshHook: rec.hook(),
	}
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
	rm := &RouteManager{
		vrfName:     "vrf-provider",
		vethNexthop: "169.254.0.1",
		execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("err"), errors.New("vtysh failed")
		},
	}
	if err := rm.DelFRRRoutes([]string{"10.0.0.1"}); err == nil {
		t.Fatal("expected error when vtysh exec fails, got nil")
	}
}

func TestRefreshBGPInvokesVtysh(t *testing.T) {
	rec := newVtyshRecorder()
	rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}
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
	rm := &RouteManager{
		vrfName: "vrf-provider",
		execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("err"), errors.New("vtysh failed")
		},
	}
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
			`ZEBRA: ip prefix-list ANNOUNCED-NETWORKS: 2 entries
   seq 5 permit 198.51.100.0/24 ge 32 le 32
   seq 10 permit 203.0.113.0/24 ge 32 le 32
`,
			[]prefixListEntry{
				{Seq: 5, Network: "198.51.100.0/24"},
				{Seq: 10, Network: "203.0.113.0/24"},
			},
		},
		{
			"non-managed entries are skipped (no ge 32 le 32)",
			`   seq 5 permit 198.51.100.0/24
   seq 10 deny 10.0.0.0/8 ge 32 le 32
`,
			nil,
		},
		{"empty output", "", nil},
		{"Can't find message returns nil", "% Can't find specified prefix-list\n", nil},
		{
			"malformed seq number is skipped",
			"   seq notanumber permit 198.51.100.0/24 ge 32 le 32\n",
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newVtyshRecorder()
			rec.on(
				[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS"},
				tt.output, nil,
			)
			rm := &RouteManager{
				frrPrefixList: "ANNOUNCED-NETWORKS",
				execVtyshHook: rec.hook(),
			}
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

func TestListFRRPrefixListEntries_PropagatesVtyshError(t *testing.T) {
	rm := &RouteManager{
		frrPrefixList: "ANNOUNCED-NETWORKS",
		execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("err"), errors.New("vtysh failed")
		},
	}
	if _, err := rm.ListFRRPrefixListEntries(); err == nil {
		t.Fatal("expected error when vtysh fails, got nil")
	}
}

func TestReconcileFRRPrefixList_AddsMissingAndRemovesStale(t *testing.T) {
	rec := newVtyshRecorder()
	// Initial state has 10.0.0.0/24 (stale) and 198.51.100.0/24 (desired).
	rec.on(
		[]string{"vtysh", "-c", "show ip prefix-list ANNOUNCED-NETWORKS"},
		`   seq 5 permit 10.0.0.0/24 ge 32 le 32
   seq 10 permit 198.51.100.0/24 ge 32 le 32
`,
		nil,
	)

	rm := &RouteManager{frrPrefixList: "ANNOUNCED-NETWORKS", execVtyshHook: rec.hook()}

	_, desired1, _ := net.ParseCIDR("198.51.100.0/24")
	_, desired2, _ := net.ParseCIDR("203.0.113.0/24") // new
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{desired1, desired2}); err != nil {
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

func TestReconcileFRRPrefixList_AddFailureBailsOut(t *testing.T) {
	calls := 0
	rm := &RouteManager{
		frrPrefixList: "ANNOUNCED-NETWORKS",
		execVtyshHook: func(cmd *exec.Cmd) ([]byte, error) {
			calls++
			joined := strings.Join(cmd.Args, " ")
			if strings.Contains(joined, "show ip prefix-list") {
				return nil, nil
			}
			return []byte("error output"), errors.New("vtysh add failed")
		},
	}
	_, n, _ := net.ParseCIDR("198.51.100.0/24")
	if err := rm.ReconcileFRRPrefixList([]*net.IPNet{n}); err == nil {
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
