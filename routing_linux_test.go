package main

import (
	"errors"
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
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	if err := rm.EnsureBridgeIP("not-an-ip"); err == nil {
		t.Fatal("EnsureBridgeIP(invalid) should return error")
	}
}

func TestEnsureBridgeIPWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.EnsureBridgeIP("169.254.169.254")
	if err == nil {
		t.Fatal("EnsureBridgeIP should error when the bridge device is missing")
	}
	if !strings.Contains(err.Error(), nonexistentBridge) {
		t.Errorf("error should mention the bridge name, got: %v", err)
	}
}

func TestRemoveBridgeIPRejectsInvalidIP(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	if err := rm.RemoveBridgeIP("not-an-ip"); err == nil {
		t.Fatal("RemoveBridgeIP(invalid) should return error")
	}
}

func TestRemoveBridgeIPWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.RemoveBridgeIP("169.254.169.254")
	if err == nil {
		t.Fatal("RemoveBridgeIP should error when the bridge device is missing")
	}
}

func TestAddKernelRouteWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.AddKernelRoute("10.0.0.1")
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

func TestDelKernelRouteWrapsLinkLookupError(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.DelKernelRoute("10.0.0.1")
	if err == nil {
		t.Fatal("DelKernelRoute should error when the bridge device is missing")
	}
}

func TestEnableProxyARPWritesProcSysOrErrors(t *testing.T) {
	// proc path: /proc/sys/net/ipv4/conf/<dev>/proxy_arp. With a synthetic
	// bridge that does not exist, the os.WriteFile call returns ENOENT and
	// the function wraps it. This exercises the error-wrap branch.
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	err := rm.EnableProxyARP()
	if err == nil {
		t.Fatal("EnableProxyARP should error when the bridge's proxy_arp sysctl is absent")
	}
}

func TestCleanupRoutingTableNoOpWhenTableIDZero(t *testing.T) {
	rm := &RouteManager{routeTableID: 0}
	if err := rm.CleanupRoutingTable(); err != nil {
		t.Errorf("CleanupRoutingTable with table 0 should be a no-op, got: %v", err)
	}
}

func TestCleanupRoutingTableDryRun(t *testing.T) {
	rm := &RouteManager{routeTableID: 100, dryRun: true}
	if err := rm.CleanupRoutingTable(); err != nil {
		t.Errorf("CleanupRoutingTable in dry-run should not error, got: %v", err)
	}
}

func TestGetBridgeMACReturnsErrorForMissingBridge(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	if _, err := rm.GetBridgeMAC(); err == nil {
		t.Fatal("GetBridgeMAC should error when the bridge is missing")
	}
}

func TestCheckBridgeDeviceDryRunSkips(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge, dryRun: true}
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
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	_, _, err := rm.EnsureSegmentInterface(101)
	if err == nil {
		t.Fatal("EnsureSegmentInterface should error when the bridge device is missing")
	}
	if !strings.Contains(err.Error(), nonexistentBridge) {
		t.Errorf("error should mention the bridge name, got: %v", err)
	}
}

func TestEnsureSegmentInterfaceRejectsOverlongName(t *testing.T) {
	rm := &RouteManager{bridgeDev: "br-provider-x"}
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
	rm := &RouteManager{bridgeDev: nonexistentBridge}
	if err := rm.PruneSegmentInterfaces(nil); err == nil {
		t.Fatal("PruneSegmentInterfaces should error when the bridge device is missing")
	}
}

func TestTeardownSegmentInterfacesDryRun(t *testing.T) {
	rm := &RouteManager{bridgeDev: nonexistentBridge, dryRun: true}
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
