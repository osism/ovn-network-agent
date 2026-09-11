package main

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/vishvananda/netlink"
)

// nonexistentBridge is a synthetic interface name that is guaranteed not to
// exist on any CI host, so netlink.LinkByName fails predictably and exercises
// the "find bridge X: ..." error-wrap branches in routing_linux.go without
// touching real network state.
const nonexistentBridge = "ovnagent-nonexistent-br"

func TestEnsureBridgeIPRejectsInvalidIP(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	if err := rm.EnsureBridgeIP("not-an-ip"); err == nil {
		t.Fatal("EnsureBridgeIP(invalid) should return error")
	}
}

func TestEnsureBridgeIPWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	err := rm.EnsureBridgeIP("169.254.169.254")
	if err == nil {
		t.Fatal("EnsureBridgeIP should error when the bridge device is missing")
	}
	if !strings.Contains(err.Error(), nonexistentBridge) {
		t.Errorf("error should mention the bridge name, got: %v", err)
	}
}

// A VRF the agent cannot look up is an unanswerable question, not a missing
// default route: the caller turns a false into an operator-facing outage
// report, so the lookup failure has to surface as an error instead.
func TestVRFDefaultRoutePresentWrapsVRFLookupError(t *testing.T) {
	rm := &RouteManager{cfg: Config{VRFName: nonexistentBridge}}
	present, err := rm.VRFDefaultRoutePresent()
	if err == nil {
		t.Fatal("VRFDefaultRoutePresent should error when the VRF device is missing")
	}
	if present {
		t.Error("VRFDefaultRoutePresent should not report a default route it could not look up")
	}
	if !strings.Contains(err.Error(), nonexistentBridge) {
		t.Errorf("error should mention the VRF name, got: %v", err)
	}
}

// A VRF that routes several networks but has no way out of them is the state
// the whole check exists to name: the table is full, and every destination
// outside it is still dropped.
//
// The cases below carry the encoding RouteListFiltered actually returns, not
// the one the write side accepts. Those differ, and an earlier version of this
// test asserted the write-side shape only — so it passed while the check read
// every real routing table as empty of defaults.
func TestHasDefaultRoute(t *testing.T) {
	_, provider, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatalf("parse provider CIDR: %v", err)
	}
	_, fip, err := net.ParseCIDR("192.0.2.10/32")
	if err != nil {
		t.Fatalf("parse FIP CIDR: %v", err)
	}
	// What the deserializer hands back for `default via …`: an explicit
	// 0.0.0.0/0 with a zero-length mask, verified against netlink v1.3.1
	// reading a real table 100.
	_, defaultDst, err := net.ParseCIDR("0.0.0.0/0")
	if err != nil {
		t.Fatalf("parse default CIDR: %v", err)
	}

	tests := []struct {
		name   string
		routes []netlink.Route
		want   bool
	}{
		{name: "an empty table has no default"},
		{
			name:   "a populated table without a default",
			routes: []netlink.Route{{Dst: provider}, {Dst: fip}},
		},
		{
			// 186 is RTPROT_BGP, which the syscall package does not name. The
			// protocol is not part of the question — only the prefix is.
			name: "a BGP-learned default, as netlink returns it",
			routes: []netlink.Route{
				{Dst: provider},
				{Dst: defaultDst, Protocol: netlink.RouteProtocol(186)},
			},
			want: true,
		},
		{
			name:   "a statically configured default counts too",
			routes: []netlink.Route{{Dst: defaultDst, Protocol: netlink.RouteProtocol(syscall.RTPROT_STATIC)}},
			want:   true,
		},
		{
			// The write-side shape, which SetupVethLeak uses for the leak
			// table. Kept so the check survives a library that normalises the
			// other way.
			name:   "a default with Dst left unset",
			routes: []netlink.Route{{Dst: nil}},
			want:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasDefaultRoute(tc.routes); got != tc.want {
				t.Errorf("hasDefaultRoute() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Dry-run never touches the system, so it cannot answer the question either —
// but reporting the dependency as satisfied is the quiet answer, matching
// VethNexthopResolvable.
func TestVRFDefaultRoutePresentIsQuietInDryRun(t *testing.T) {
	rm := &RouteManager{cfg: Config{VRFName: nonexistentBridge, DryRun: true}}
	present, err := rm.VRFDefaultRoutePresent()
	if err != nil {
		t.Fatalf("VRFDefaultRoutePresent in dry-run: %v", err)
	}
	if !present {
		t.Error("dry-run should report the default route as present")
	}
}

func TestRemoveBridgeIPRejectsInvalidIP(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	if err := rm.RemoveBridgeIP("not-an-ip"); err == nil {
		t.Fatal("RemoveBridgeIP(invalid) should return error")
	}
}

func TestRemoveBridgeIPWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	err := rm.RemoveBridgeIP("169.254.169.254")
	if err == nil {
		t.Fatal("RemoveBridgeIP should error when the bridge device is missing")
	}
}

func TestAddKernelRouteWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	err := rm.AddKernelRoute("10.0.0.1", nonexistentBridge)
	if err == nil {
		t.Fatal("AddKernelRoute should error when the bridge device is missing")
	}
}

func TestAddKernelRouteRejectsInvalidIP(t *testing.T) {
	// AddKernelRoute looks up the bridge before parsing the IP, so an
	// invalid IP can only be reached on a host that has the bridge —
	// skipped here. Covered by the validation in helper callers and the
	// AddFRRRoutes batch path (validateIP) elsewhere.
	t.Skip("validation happens after link lookup; covered by callers")
}

func TestLocalIPRuleSpec(t *testing.T) {
	tests := []struct {
		name                    string
		cfg                     Config
		wantTable, wantPriority int
		wantEnabled             bool
	}{
		{
			name:         "dedicated route table keeps existing selector",
			cfg:          Config{RouteTableID: 123, VethLeakEnabled: true, VethLeakRulePriority: 2000},
			wantTable:    123,
			wantPriority: 1000,
			wantEnabled:  true,
		},
		{
			name:         "main table with veth leak bypasses source policy",
			cfg:          Config{VethLeakEnabled: true, VethLeakRulePriority: 2000},
			wantTable:    rtTableMain,
			wantPriority: 1999,
			wantEnabled:  true,
		},
		{
			name:        "main table without veth leak needs no selector",
			cfg:         Config{},
			wantEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rm := &RouteManager{cfg: tt.cfg}
			table, priority, enabled := rm.localIPRuleSpec()
			if table != tt.wantTable || priority != tt.wantPriority || enabled != tt.wantEnabled {
				t.Errorf("localIPRuleSpec() = (%d, %d, %t), want (%d, %d, %t)",
					table, priority, enabled, tt.wantTable, tt.wantPriority, tt.wantEnabled)
			}
		})
	}
}

func TestDelKernelRouteWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	err := rm.DelKernelRoute("10.0.0.1", nonexistentBridge)
	if err == nil {
		t.Fatal("DelKernelRoute should error when the bridge device is missing")
	}
}

func TestEnableProxyARPWritesProcSysOrErrors(t *testing.T) {
	// proc path: /proc/sys/net/ipv4/conf/<dev>/proxy_arp. With a synthetic
	// bridge that does not exist, the os.WriteFile call returns ENOENT and
	// the function wraps it. This exercises the error-wrap branch.
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	err := rm.EnableProxyARP()
	if err == nil {
		t.Fatal("EnableProxyARP should error when the bridge's proxy_arp sysctl is absent")
	}
}

func TestCleanupRoutingTableNoOpWhenTableIDZero(t *testing.T) {
	rm := &RouteManager{cfg: Config{RouteTableID: 0}}
	if err := rm.CleanupRoutingTable(); err != nil {
		t.Errorf("CleanupRoutingTable with table 0 should be a no-op, got: %v", err)
	}
}

func TestCleanupRoutingTableDryRun(t *testing.T) {
	rm := &RouteManager{cfg: Config{RouteTableID: 100, DryRun: true}}
	if err := rm.CleanupRoutingTable(); err != nil {
		t.Errorf("CleanupRoutingTable in dry-run should not error, got: %v", err)
	}
}

func TestGetBridgeMACReturnsErrorForMissingBridge(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	if _, err := rm.GetBridgeMAC(); err == nil {
		t.Fatal("GetBridgeMAC should error when the bridge is missing")
	}
}

func TestCheckBridgeDeviceDryRunSkips(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge, DryRun: true}}
	if err := rm.CheckBridgeDevice(); err != nil {
		t.Errorf("CheckBridgeDevice in dry-run should not error, got: %v", err)
	}
}

func TestSegmentIfaceNameLength(t *testing.T) {
	tests := []struct {
		name      string
		bridgeDev string
		tag       int
		want      string
		wantErr   bool
	}{
		{"default bridge fits with max tag", "br-ex", 4094, "br-ex.4094", false},
		{"short bridge and tag", "br-ex", 101, "br-ex.101", false},
		{"long bridge with 4-digit tag exceeds IFNAMSIZ", "br-provider", 4094, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := segmentIfaceName(tt.bridgeDev, tt.tag)
			if tt.wantErr {
				if err == nil {
					t.Errorf("segmentIfaceName(%q, %d) should error", tt.bridgeDev, tt.tag)
				}
				return
			}
			if err != nil {
				t.Fatalf("segmentIfaceName(%q, %d) error: %v", tt.bridgeDev, tt.tag, err)
			}
			if got != tt.want {
				t.Errorf("segmentIfaceName(%q, %d) = %q, want %q", tt.bridgeDev, tt.tag, got, tt.want)
			}
		})
	}
}

func TestEnsureSegmentInterfaceWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	_, _, err := rm.EnsureSegmentInterface(101)
	if err == nil {
		t.Fatal("EnsureSegmentInterface should error when the bridge device is missing")
	}
	if !strings.Contains(err.Error(), nonexistentBridge) {
		t.Errorf("error should mention the bridge name, got: %v", err)
	}
}

func TestEnsureSegmentInterfaceRejectsOverlongName(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-provider-x"}}
	_, _, err := rm.EnsureSegmentInterface(4094)
	if err == nil {
		t.Fatal("EnsureSegmentInterface should reject a name over the kernel limit")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error should mention the length limit, got: %v", err)
	}
}

// TestVerifyAdoptedSegmentLink guards the adoption check: an interface named
// <bridge>.<tag> that already exists is only reused when it is really a VLAN
// subinterface with the matching tag and parent. A name collision (wrong tag,
// non-VLAN device, or a subinterface of a different bridge) must be refused so
// the segment's traffic is not steered onto the wrong interface.
func TestVerifyAdoptedSegmentLink(t *testing.T) {
	const parentIndex = 7
	tests := []struct {
		name    string
		link    netlink.Link
		tag     int
		wantErr string
	}{
		{
			name: "matching vlan is adopted",
			link: &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br-ex.101", ParentIndex: parentIndex}, VlanId: 101},
			tag:  101,
		},
		{
			name:    "wrong vlan id refused",
			link:    &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br-ex.101", ParentIndex: parentIndex}, VlanId: 999},
			tag:     101,
			wantErr: "VLAN id",
		},
		{
			name:    "wrong parent refused",
			link:    &netlink.Vlan{LinkAttrs: netlink.LinkAttrs{Name: "br-ex.101", ParentIndex: parentIndex + 1}, VlanId: 101},
			tag:     101,
			wantErr: "parent index",
		},
		{
			name:    "non-vlan device refused",
			link:    &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "br-ex.101", ParentIndex: parentIndex}},
			tag:     101,
			wantErr: "not an 802.1Q",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyAdoptedSegmentLink(tt.link, tt.tag, parentIndex)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyAdoptedSegmentLink() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyAdoptedSegmentLink() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("verifyAdoptedSegmentLink() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPruneSegmentInterfacesWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge}}
	if err := rm.PruneSegmentInterfaces(nil); err == nil {
		t.Fatal("PruneSegmentInterfaces should error when the bridge device is missing")
	}
}

func TestTeardownSegmentInterfacesDryRun(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: nonexistentBridge, DryRun: true}}
	if err := rm.TeardownSegmentInterfaces(); err != nil {
		t.Errorf("TeardownSegmentInterfaces in dry-run should not error, got: %v", err)
	}
}

func TestIsFileExists(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"EEXIST", syscall.EEXIST, true},
		{"wrapped EEXIST", &wrappedErr{syscall.EEXIST}, true},
		{"unrelated", errors.New("permission denied"), false},
		{"nil-equivalent unrelated", syscall.ENOENT, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFileExists(tt.err); got != tt.want {
				t.Errorf("isFileExists(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// wrappedErr lets the isFileExists test verify errors.Is-style unwrapping
// without depending on fmt.Errorf's %w formatting.
type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }
