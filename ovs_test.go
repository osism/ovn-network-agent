package main

import (
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ovsRecorder captures calls to RouteManager.runOVS for assertion. It returns
// canned responses keyed by the full joined command line and falls back to
// (nil, nil) — i.e. empty output, no error — for unmatched commands.
type ovsRecorder struct {
	calls     [][]string
	responses map[string]ovsResponse
}

type ovsResponse struct {
	out []byte
	err error
}

func newOVSRecorder() *ovsRecorder {
	return &ovsRecorder{responses: map[string]ovsResponse{}}
}

// on registers a canned response for a command identified by its full Args.
func (r *ovsRecorder) on(args []string, out string, err error) {
	r.responses[strings.Join(args, " ")] = ovsResponse{out: []byte(out), err: err}
}

func (r *ovsRecorder) hook() ovsExecFunc {
	return func(cmd *exec.Cmd) ([]byte, error) {
		args := append([]string{}, cmd.Args...)
		r.calls = append(r.calls, args)
		if resp, ok := r.responses[strings.Join(cmd.Args, " ")]; ok {
			return resp.out, resp.err
		}
		return nil, nil
	}
}

// findAddFlows returns just the flow strings from "ovs-ofctl add-flow <br> <flow>" calls.
func (r *ovsRecorder) findAddFlows() []string {
	var flows []string
	for _, c := range r.calls {
		if len(c) >= 4 && c[0] == "ovs-ofctl" && c[1] == "add-flow" {
			flows = append(flows, c[3])
		}
	}
	return flows
}

// containsFlow reports whether want is one of the recorded flow strings.
func containsFlow(flows []string, want string) bool {
	for _, f := range flows {
		if f == want {
			return true
		}
	}
	return false
}

func TestHairpinFlow(t *testing.T) {
	tests := []struct {
		name      string
		cookie    string
		ofport    string
		ip        string
		bridgeMAC string
		routerMAC string
		ipv6      bool
		want      string
	}{
		{
			"basic IPv4 hairpin flow",
			"0x998", "42", "5.182.234.199", "aa:bb:cc:dd:ee:ff", "fa:16:3e:6f:a1:64", false,
			"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port",
		},
		{
			"different IPv4 IP and ofport",
			"0x998", "7", "192.0.2.1", "11:22:33:44:55:66", "fa:16:3e:ab:cd:ef", false,
			"cookie=0x998,priority=910,ip,in_port=7,ip_dst=192.0.2.1/32,actions=mod_dl_src:11:22:33:44:55:66,mod_dl_dst:fa:16:3e:ab:cd:ef,output:in_port",
		},
		{
			"SNAT router external IP",
			"0x998", "3", "5.182.234.128", "82:ba:92:54:47:48", "fa:16:3e:45:06:3e", false,
			"cookie=0x998,priority=910,ip,in_port=3,ip_dst=5.182.234.128/32,actions=mod_dl_src:82:ba:92:54:47:48,mod_dl_dst:fa:16:3e:45:06:3e,output:in_port",
		},
		{
			"IPv6 FIP",
			"0x998", "42", "2001:db8::1", "aa:bb:cc:dd:ee:ff", "fa:16:3e:00:00:01", true,
			"cookie=0x998,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,output:in_port",
		},
		{
			"IPv6 SNAT",
			"0x998", "5", "2001:db8:cafe::1", "aa:bb:cc:dd:ee:ff", "fa:16:3e:00:00:02", true,
			"cookie=0x998,priority=910,ipv6,in_port=5,ipv6_dst=2001:db8:cafe::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:02,output:in_port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HairpinFlow(tt.cookie, tt.ofport, tt.ip, tt.bridgeMAC, tt.routerMAC, tt.ipv6)
			if got != tt.want {
				t.Errorf("HairpinFlow() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMACTweakFlow(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		ofport string
		mac    string
		ipv6   bool
		want   string
	}{
		{
			"IPv4 flow",
			"0x999", "42", "aa:bb:cc:dd:ee:ff", false,
			"cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		},
		{
			"IPv6 flow",
			"0x999", "42", "aa:bb:cc:dd:ee:ff", true,
			"cookie=0x999,priority=900,ipv6,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		},
		{
			"different ofport and MAC",
			"0x999", "7", "11:22:33:44:55:66", false,
			"cookie=0x999,priority=900,ip,in_port=7,actions=mod_dl_dst:11:22:33:44:55:66,NORMAL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MACTweakFlow(tt.cookie, tt.ofport, tt.mac, tt.ipv6)
			if got != tt.want {
				t.Errorf("MACTweakFlow() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOVSCmdWrapperPrepended(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, ovsWrapper: []string{"docker", "exec", "openvswitch_vswitchd"}}

	cmd := rm.ovsCmd("ovs-ofctl", "add-flow", "br-ex", "flow")
	want := []string{"docker", "exec", "openvswitch_vswitchd", "ovs-ofctl", "add-flow", "br-ex", "flow"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("ovsCmd args = %v, want %v", cmd.Args, want)
	}
}

func TestOVSCmdNoWrapper(t *testing.T) {
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}}

	cmd := rm.ovsCmd("ovs-ofctl", "add-flow", "br-ex", "flow")
	want := []string{"ovs-ofctl", "add-flow", "br-ex", "flow"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("ovsCmd args = %v, want %v", cmd.Args, want)
	}
}

// fallbackSegments returns a segment map holding only the legacy
// single-patch-port fallback binding, as EnsureSegments resolves it for a
// flat single-network deployment.
func fallbackSegments(patchPort, ofport, mac string) map[string]*segmentBinding {
	return map[string]*segmentBinding{
		"": {patchPort: patchPort, ofport: ofport, kernelDev: "br-ex", kernelMAC: mac},
	}
}

// TestEnsureSegmentsWithCachedFallbackKeepsFlatFlowSet locks the flat
// single-network behavior: with a cached fallback binding that still
// resolves to its ofport, EnsureSegments issues exactly the pre-segment
// command sequence — one ofport validation, one cookie sweep, and the
// IPv4+IPv6 MAC-tweak pair for the single patch port and bridge MAC.
func TestEnsureSegmentsWithCachedFallbackKeepsFlatFlowSet(t *testing.T) {
	rec := newOVSRecorder()
	// EnsureSegments re-validates every cached binding on each call; here
	// the patch port still resolves to ofport 42, so no rediscovery runs.
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"42\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}}); err != nil {
		t.Fatalf("EnsureSegments() error: %v", err)
	}

	// Expect: 1 ofport validation + 1 del-flows + 2 add-flows (IPv4 + IPv6).
	if len(rec.calls) != 4 {
		t.Fatalf("expected 4 OVS commands, got %d: %v", len(rec.calls), rec.calls)
	}

	wantValidate := []string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"}
	if !reflect.DeepEqual(rec.calls[0], wantValidate) {
		t.Errorf("first call = %v, want %v", rec.calls[0], wantValidate)
	}
	wantDel := []string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"}
	if !reflect.DeepEqual(rec.calls[1], wantDel) {
		t.Errorf("second call = %v, want %v", rec.calls[1], wantDel)
	}

	flows := rec.findAddFlows()
	wantFlows := []string{
		"cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
	}
	if !reflect.DeepEqual(flows, wantFlows) {
		t.Errorf("add-flow flows = %v, want %v", flows, wantFlows)
	}
}

// TestEnsureSegmentsInstallsFlowsPerPatchPort covers the multi-VLAN path:
// two localnet segments resolve to their own patch ports (via the
// external_ids ovn-controller stamps on the Port rows) and their own kernel
// interfaces (via the segment-interface hook), and the MAC-tweak flows are
// installed per patch port with the per-segment MACs after a single
// bridge-wide cookie sweep.
func TestEnsureSegmentsInstallsFlowsPerPatchPort(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"patch-seg101\npatch-seg102\nphy-eth0\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-seg101", "external_ids:ovn-localnet-port"},
		"\"seg-101\"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-seg102", "external_ids:ovn-localnet-port"},
		"\"seg-102\"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "phy-eth0", "external_ids:ovn-localnet-port"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-seg101", "ofport"},
		"5\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-seg102", "ofport"},
		"6\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook(), segmentIfaceHook: func(tag int) (string, string, error) {
		return fmt.Sprintf("br-ex.%d", tag), fmt.Sprintf("aa:bb:cc:dd:ee:%02x", tag), nil
	}}

	tag101, tag102 := 101, 102
	desired := []DesiredSegment{
		{LocalnetPort: "seg-101", VLANTag: &tag101},
		{LocalnetPort: "seg-102", VLANTag: &tag102},
	}
	if err := rm.EnsureSegments(desired); err != nil {
		t.Fatalf("EnsureSegments() error: %v", err)
	}

	flows := rec.findAddFlows()
	wantFlows := []string{
		"cookie=0x999,priority=900,ip,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL",
		"cookie=0x999,priority=900,ip,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
	}
	if !reflect.DeepEqual(flows, wantFlows) {
		t.Errorf("add-flow flows = %v, want %v", flows, wantFlows)
	}

	// Exactly one cookie sweep, bridge-wide.
	dels := 0
	for _, c := range rec.calls {
		if len(c) >= 4 && c[1] == "del-flows" && c[3] == "cookie=0x999/-1" {
			dels++
		}
	}
	if dels != 1 {
		t.Errorf("expected 1 del-flows sweep, got %d", dels)
	}

	// The kernel devices are recorded per segment for the routing side.
	if got := rm.SegmentDev("seg-101"); got != "br-ex.101" {
		t.Errorf("SegmentDev(seg-101) = %q, want br-ex.101", got)
	}
	if got := rm.SegmentMAC("seg-102"); got != "aa:bb:cc:dd:ee:66" {
		t.Errorf("SegmentMAC(seg-102) = %q, want aa:bb:cc:dd:ee:66", got)
	}
}

// TestEnsureSegmentsSkipsSegmentWhenIfaceFails exercises the error path of
// the kernel-interface resolution (e.g. an interface name over the IFNAMSIZ
// limit): the failing segment is skipped with an error log, and the other
// segment's flows are still installed.
func TestEnsureSegmentsSkipsSegmentWhenIfaceFails(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"patch-seg101\npatch-seg102\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-seg101", "external_ids:ovn-localnet-port"},
		"\"seg-101\"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-seg102", "external_ids:ovn-localnet-port"},
		"\"seg-102\"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-seg101", "ofport"},
		"5\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-seg102", "ofport"},
		"6\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook(), segmentIfaceHook: func(tag int) (string, string, error) {
		if tag == 102 {
			return "", "", errors.New("interface name too long")
		}
		return fmt.Sprintf("br-ex.%d", tag), "aa:bb:cc:dd:ee:65", nil
	}}

	tag101, tag102 := 101, 102
	desired := []DesiredSegment{
		{LocalnetPort: "seg-101", VLANTag: &tag101},
		{LocalnetPort: "seg-102", VLANTag: &tag102},
	}
	if err := rm.EnsureSegments(desired); err != nil {
		t.Fatalf("EnsureSegments() error: %v", err)
	}

	flows := rec.findAddFlows()
	if len(flows) != 2 {
		t.Fatalf("expected 2 flows (healthy segment only), got %v", flows)
	}
	for _, f := range flows {
		if !strings.Contains(f, "in_port=5") {
			t.Errorf("flow %q should target the healthy segment's in_port=5", f)
		}
	}
	if _, ok := rm.segments["seg-102"]; ok {
		t.Error("failing segment must not be recorded as bound")
	}
}

// TestEnsureSegmentsFallbackDiscoveryWhenUncached exercises the first-call
// fallback path: no cached bindings and no external_ids on any port, so the
// desired segment falls back to the legacy single-patch-port discovery
// (discoverPatchPort + getOFPort) and only falls over at GetBridgeMAC
// (which has no hook). The discovery branches are covered up to that point,
// and the function returns the wrapped MAC error.
func TestEnsureSegmentsFallbackDiscoveryWhenUncached(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "ovnagent-nonexistent-br"},
		"phy-eth0\npatch-provnet-0\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "phy-eth0", "external_ids:ovn-localnet-port"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-provnet-0", "external_ids:ovn-localnet-port"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "phy-eth0", "type"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "patch-provnet-0", "type"},
		"patch\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"42\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "ovnagent-nonexistent-br"}, execOVSHook: rec.hook()}

	err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}})
	if err == nil {
		t.Fatal("expected GetBridgeMAC error in absence of a real bridge")
	}
	if !strings.Contains(err.Error(), "get bridge MAC") {
		t.Errorf("expected 'get bridge MAC' wrapped error, got: %v", err)
	}
	// Discovery commands must have been dispatched before the MAC lookup failed.
	if len(rm.segments) != 0 {
		t.Errorf("bindings should remain empty when discovery aborts: %v", rm.segments)
	}
}

func TestEnsureSegmentsTolersDelFailure(t *testing.T) {
	// del-flows is treated as best-effort; a failure must not abort the
	// subsequent add-flow calls.
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"42\n", nil,
	)
	rec.on(
		[]string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"},
		"some output", errors.New("transient ofctl error"),
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}}); err != nil {
		t.Fatalf("EnsureSegments() should swallow del-flows error, got: %v", err)
	}

	if got := len(rec.findAddFlows()); got != 2 {
		t.Errorf("expected 2 add-flow calls after del failure, got %d", got)
	}
}

// TestEnsureSegmentsAddFailureDoesNotStarveOthers verifies that a per-segment
// add-flow failure is logged and skipped rather than aborting the whole loop.
// All flows are deleted up front, so returning on the first failure would
// leave every later segment with no MAC-tweak flow at all. Here the first
// segment's IPv4 add-flow fails; the second segment's flows must still be
// installed and EnsureSegments must not report a hard error.
func TestEnsureSegmentsAddFailureDoesNotStarveOthers(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"patch-seg101\npatch-seg102\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-seg101", "external_ids:ovn-localnet-port"},
		"\"seg-101\"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-seg102", "external_ids:ovn-localnet-port"},
		"\"seg-102\"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-seg101", "ofport"},
		"5\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-seg102", "ofport"},
		"6\n", nil,
	)
	// The first segment's IPv4 add-flow fails.
	rec.on(
		[]string{"ovs-ofctl", "add-flow", "br-ex",
			"cookie=0x999,priority=900,ip,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL"},
		"add-flow failed", errors.New("bad flow"),
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook(), segmentIfaceHook: func(tag int) (string, string, error) {
		return fmt.Sprintf("br-ex.%d", tag), fmt.Sprintf("aa:bb:cc:dd:ee:%02x", tag), nil
	}}

	tag101, tag102 := 101, 102
	desired := []DesiredSegment{
		{LocalnetPort: "seg-101", VLANTag: &tag101},
		{LocalnetPort: "seg-102", VLANTag: &tag102},
	}
	if err := rm.EnsureSegments(desired); err != nil {
		t.Fatalf("EnsureSegments() should tolerate a per-segment add-flow failure, got: %v", err)
	}

	// The healthy second segment's flows must still have been installed.
	flows := rec.findAddFlows()
	wantSurviving := []string{
		"cookie=0x999,priority=900,ip,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
	}
	for _, want := range wantSurviving {
		if !containsFlow(flows, want) {
			t.Errorf("healthy segment flow %q not installed after earlier failure; got %v", want, flows)
		}
	}
}

// TestEnsureSegmentsRediscoversWhenOfportChanges covers the core of the
// patch-port staleness fix: the cached patch port now resolves to a different
// ofport (as if ovn-controller had recreated it). EnsureSegments must drop
// the stale bindings and rediscover, rather than keep installing flows for a
// dead in_port. Rediscovery runs through the external_ids mapping, falls
// back to discoverPatchPort + getOFPort, and then fails at GetBridgeMAC (no
// real bridge in a unit test) — which proves the stale cache was abandoned
// instead of trusted.
func TestEnsureSegmentsRediscoversWhenOfportChanges(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"99\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"patch-provnet-0\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-provnet-0", "external_ids:ovn-localnet-port"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "patch-provnet-0", "type"},
		"patch\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}})
	if err == nil || !strings.Contains(err.Error(), "get bridge MAC") {
		t.Fatalf("expected rediscovery to reach GetBridgeMAC, got: %v", err)
	}
	if len(rm.segments) != 0 {
		t.Errorf("stale bindings must be cleared on rediscovery: %v", rm.segments)
	}
}

// TestEnsureSegmentsRediscoversWhenSegmentSetChanges verifies that a change
// in the desired segment set (a second network appearing on the node)
// triggers rediscovery even though the cached binding's ofport is unchanged.
func TestEnsureSegmentsRediscoversWhenSegmentSetChanges(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"patch-seg101\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Port", "patch-seg101", "external_ids:ovn-localnet-port"},
		"\"seg-101\"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-seg101", "ofport"},
		"5\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook(), segmentIfaceHook: func(tag int) (string, string, error) {
		return fmt.Sprintf("br-ex.%d", tag), "aa:bb:cc:dd:ee:65", nil
	}}

	tag101 := 101
	if err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: "seg-101", VLANTag: &tag101}}); err != nil {
		t.Fatalf("EnsureSegments() error: %v", err)
	}
	if _, ok := rm.segments["seg-101"]; !ok {
		t.Errorf("expected binding for seg-101 after set change, got %v", rm.segments)
	}
	if _, ok := rm.segments[""]; ok {
		t.Errorf("stale fallback binding should be gone, got %v", rm.segments)
	}
}

func TestReconcileOVSHairpinFlowsInstallsExpectedFlows(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
		"2001:db8::1":   {RouterMAC: "fa:16:3e:00:00:01"},
	}

	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	// First call should be del-flows for the hairpin cookie.
	wantDel := []string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x998/-1"}
	if !reflect.DeepEqual(rec.calls[0], wantDel) {
		t.Errorf("first call = %v, want %v", rec.calls[0], wantDel)
	}

	// Two add-flows expected, one per IP. Order is map-iteration-dependent,
	// so compare as sets.
	got := rec.findAddFlows()
	sort.Strings(got)
	want := []string{
		"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port",
		"cookie=0x998,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,output:in_port",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("add-flow flows = %v, want %v", got, want)
	}
}

// TestReconcileOVSHairpinFlowsPerSegment verifies that each IP's hairpin
// flow binds to its own segment's patch port and rewrites dl_src to that
// segment's kernel MAC, so reflection stays within the segment. An IP whose
// segment has no binding (and no fallback exists) is skipped rather than
// bound to the wrong port.
func TestReconcileOVSHairpinFlowsPerSegment(t *testing.T) {
	rec := newOVSRecorder()
	tag101, tag102 := 101, 102
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: map[string]*segmentBinding{
		"seg-101": {patchPort: "patch-seg101", ofport: "5", kernelDev: "br-ex.101", kernelMAC: "aa:bb:cc:dd:ee:65", vlanTag: &tag101},
		"seg-102": {patchPort: "patch-seg102", ofport: "6", kernelDev: "br-ex.102", kernelMAC: "aa:bb:cc:dd:ee:66", vlanTag: &tag102},
	}, execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"198.51.100.50": {RouterMAC: "fa:16:3e:00:01:01", Segment: "seg-101"},
		"203.0.113.50":  {RouterMAC: "fa:16:3e:00:01:02", Segment: "seg-102"},
		// Unresolved segment and no "" fallback binding — must be skipped.
		"192.0.2.50": {RouterMAC: "fa:16:3e:00:01:03", Segment: ""},
	}

	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	got := rec.findAddFlows()
	sort.Strings(got)
	want := []string{
		"cookie=0x998,priority=910,ip,in_port=5,ip_dst=198.51.100.50/32,actions=mod_dl_src:aa:bb:cc:dd:ee:65,mod_dl_dst:fa:16:3e:00:01:01,output:in_port",
		"cookie=0x998,priority=910,ip,in_port=6,ip_dst=203.0.113.50/32,actions=mod_dl_src:aa:bb:cc:dd:ee:66,mod_dl_dst:fa:16:3e:00:01:02,output:in_port",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("add-flow flows = %v, want %v", got, want)
	}
}

func TestReconcileOVSHairpinFlowsEmptyMapClearsAll(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(nil); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows(nil) error: %v", err)
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected only del-flows call, got %d: %v", len(rec.calls), rec.calls)
	}
	if rec.calls[0][1] != "del-flows" {
		t.Errorf("expected del-flows, got %v", rec.calls[0])
	}
}

func TestReconcileOVSHairpinFlowsNoBindingsIsNoOp(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{
		cfg:         Config{BridgeDev: "br-ex"},
		execOVSHook: rec.hook(),
		// segments intentionally empty.
	}

	if err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{"10.0.0.1": {RouterMAC: "aa:aa:aa:aa:aa:aa"}}); err != nil {
		t.Fatalf("expected no-op when bindings are empty, got: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected no OVS commands when bindings empty, got: %v", rec.calls)
	}
}

// TestReconcileOVSHairpinFlowsSkipsInvalidIP proves an invalid IP is skipped
// with no error and does not prevent the healthy flow from installing (issue
// #158 test b, hairpin half). The old behaviour deleted every flow up front
// then aborted on the first invalid IP, leaving the plane permanently empty.
func TestReconcileOVSHairpinFlowsSkipsInvalidIP(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"not-an-ip":     {RouterMAC: "aa:aa:aa:aa:aa:aa"},
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
	}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() must skip the invalid IP, got: %v", err)
	}

	wantHealthy := "cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port"
	if !containsFlow(rec.findAddFlows(), wantHealthy) {
		t.Errorf("healthy flow not installed despite the invalid sibling; got %v", rec.findAddFlows())
	}
}

// TestReconcileOVSHairpinFlowsAddFailureDoesNotStarveOthers proves a failed
// add-flow for one FIP does not abort the loop: the other FIP's flow still
// installs and the call returns nil (issue #158 test b). Mirrors
// TestEnsureSegmentsAddFailureDoesNotStarveOthers.
func TestReconcileOVSHairpinFlowsAddFailureDoesNotStarveOthers(t *testing.T) {
	rec := newOVSRecorder()
	// The flow for the first FIP fails to install.
	const failingFlow = "cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port"
	rec.on([]string{"ovs-ofctl", "add-flow", "br-ex", failingFlow}, "add-flow failed", errors.New("bad flow"))

	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
		"203.0.113.50":  {RouterMAC: "fa:16:3e:00:01:02"},
	}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() must tolerate a per-flow add failure, got: %v", err)
	}

	wantSurviving := "cookie=0x998,priority=910,ip,in_port=42,ip_dst=203.0.113.50/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:01:02,output:in_port"
	if !containsFlow(rec.findAddFlows(), wantSurviving) {
		t.Errorf("healthy flow not installed after the sibling's add-flow failure; got %v", rec.findAddFlows())
	}
}

func TestReconcileOVSHairpinFlowsDryRun(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", DryRun: true}, execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{"10.0.0.1": {RouterMAC: "aa:aa:aa:aa:aa:aa"}}); err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Errorf("dry-run should issue no commands, got: %v", rec.calls)
	}
}

func TestRemoveOVSFlowsIssuesBothDeletes(t *testing.T) {
	rec := newOVSRecorder()
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook()}

	if err := rm.RemoveOVSFlows(); err != nil {
		t.Fatalf("RemoveOVSFlows() error: %v", err)
	}

	want := [][]string{
		{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"},
		{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x998/-1"},
	}
	if !reflect.DeepEqual(rec.calls, want) {
		t.Errorf("RemoveOVSFlows() calls = %v, want %v", rec.calls, want)
	}
}

func TestRemoveOVSFlowsMACTweakFailureStops(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-ofctl", "del-flows", "br-ex", "cookie=0x999/-1"},
		"err output", errors.New("ofctl exit 1"),
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook()}

	if err := rm.RemoveOVSFlows(); err == nil {
		t.Fatal("expected error when MAC-tweak del-flows fails")
	}
	// The hairpin del-flows must NOT run after the first failure.
	if len(rec.calls) != 1 {
		t.Errorf("expected 1 call before bail-out, got %d: %v", len(rec.calls), rec.calls)
	}
}

func TestDiscoverPatchPortFindsPatchType(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"phy-eth0\npatch-provnet-0\nphy-eth1\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "phy-eth0", "type"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "patch-provnet-0", "type"},
		"patch\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook()}

	port, err := rm.discoverPatchPort()
	if err != nil {
		t.Fatalf("discoverPatchPort() error: %v", err)
	}
	if port != "patch-provnet-0" {
		t.Errorf("port = %q, want %q", port, "patch-provnet-0")
	}
}

func TestDiscoverPatchPortNoPatchFound(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "list-ports", "br-ex"},
		"phy-eth0\nphy-eth1\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "phy-eth0", "type"},
		"\n", nil,
	)
	rec.on(
		[]string{"ovs-vsctl", "--if-exists", "get", "Interface", "phy-eth1", "type"},
		"\n", nil,
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook()}

	if _, err := rm.discoverPatchPort(); err == nil {
		t.Error("expected error when no patch port present")
	}
}

func TestGetOFPortRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantErr bool
		want    string
	}{
		{"valid ofport", "42\n", false, "42"},
		{"empty ofport", "\n", true, ""},
		{"unassigned ofport", "-1\n", true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newOVSRecorder()
			rec.on(
				[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
				tt.out, nil,
			)
			rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook()}
			got, err := rm.getOFPort("patch-provnet-0")
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for output %q", tt.out)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ofport = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunOVSWithoutHookCallsCombinedOutput(t *testing.T) {
	// Sanity check that runOVS without a hook does run a real command.
	// /usr/bin/true (or "true" on PATH) is a portable success exit; on macOS
	// and Linux build agents this should always be present.
	rm := &RouteManager{}
	cmd := rm.ovsCmd("true")
	if cmd.Args[0] != "true" {
		t.Skipf("unexpected command shape: %v", cmd.Args)
	}
	out, err := rm.runOVS("true")
	if err != nil {
		// Some hermetic CI environments lack /bin/true; treat as skip.
		t.Skipf("`true` binary unavailable in test env: %v (output: %s)", err, out)
	}
}

// Ensure the recorder's own behavior is correct so test failures don't
// stem from a buggy harness.
func TestOVSRecorderRecordsAndResponds(t *testing.T) {
	rec := newOVSRecorder()
	rec.on([]string{"ovs-vsctl", "list-ports", "br-ex"}, "p0\np1\n", nil)
	hook := rec.hook()

	out, err := hook(exec.Command("ovs-vsctl", "list-ports", "br-ex"))
	if err != nil {
		t.Fatalf("recorder hook err: %v", err)
	}
	if string(out) != "p0\np1\n" {
		t.Errorf("recorder out = %q, want %q", string(out), "p0\np1\n")
	}

	if len(rec.calls) != 1 {
		t.Fatalf("expected 1 recorded call, got %d", len(rec.calls))
	}
	want := []string{"ovs-vsctl", "list-ports", "br-ex"}
	if !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("recorded call = %v, want %v", rec.calls[0], want)
	}
}

// Compile-time check that the recorder hook has the right signature.
var _ ovsExecFunc = (&ovsRecorder{}).hook()
