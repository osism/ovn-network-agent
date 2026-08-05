package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// ovsRecorder captures calls to RouteManager.runOVS and runOVSStdin for
// assertion. It returns canned responses keyed by the full joined command line
// and falls back to (nil, nil) — i.e. empty output, no error — for unmatched
// commands.
type ovsRecorder struct {
	calls     [][]string
	stdins    []string
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

// onStdin registers a canned response for a command identified by its Args
// *and* the exact bytes on its stdin. The bundle probe and a real bundled add
// share the argv "ovs-ofctl --bundle add-flows <br> -" and differ only in what
// they pipe in, so a test that fails one without the other needs this key.
func (r *ovsRecorder) onStdin(args []string, stdin, out string, err error) {
	r.responses[stdinKey(args, stdin)] = ovsResponse{out: []byte(out), err: err}
}

// onDump registers the dump a plane's reconcile reads back.
func (r *ovsRecorder) onDump(bridge, cookie, out string) {
	r.on([]string{"ovs-ofctl", "--no-stats", "dump-flows", bridge, "cookie=" + cookie + "/-1"}, out, nil)
}

func stdinKey(args []string, stdin string) string {
	return strings.Join(args, " ") + "\x00" + stdin
}

func (r *ovsRecorder) hook() ovsExecFunc {
	return func(cmd *exec.Cmd) ([]byte, error) {
		args := append([]string{}, cmd.Args...)
		stdin := ""
		if cmd.Stdin != nil {
			b, err := io.ReadAll(cmd.Stdin)
			if err != nil {
				return nil, err
			}
			stdin = string(b)
		}
		r.calls = append(r.calls, args)
		r.stdins = append(r.stdins, stdin)
		if resp, ok := r.responses[stdinKey(args, stdin)]; ok {
			return resp.out, resp.err
		}
		if resp, ok := r.responses[strings.Join(args, " ")]; ok {
			return resp.out, resp.err
		}
		return nil, nil
	}
}

// ofctlArgs returns a recorded call from its "ovs-ofctl" token onwards, or nil
// when the call is not an ovs-ofctl one. It exists so the helpers below read
// the same whether or not an OVS wrapper prefixes the argv.
func ofctlArgs(call []string) []string {
	for i, a := range call {
		if a == "ovs-ofctl" {
			return call[i:]
		}
	}
	return nil
}

// findAddFlows returns just the flow strings from "ovs-ofctl add-flow <br> <flow>" calls.
func (r *ovsRecorder) findAddFlows() []string {
	var flows []string
	for _, c := range r.calls {
		if a := ofctlArgs(c); len(a) >= 4 && a[1] == "add-flow" {
			flows = append(flows, a[3])
		}
	}
	return flows
}

// findBatchedFlows returns the flow specs piped into "ovs-ofctl add-flows
// <br> -" calls, in the order they were written. The empty-stdin bundle probe
// contributes nothing.
func (r *ovsRecorder) findBatchedFlows() []string {
	var flows []string
	for i, c := range r.calls {
		if ofctlArgs(c) == nil || !strings.Contains(strings.Join(c, " "), " add-flows ") {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(r.stdins[i]), "\n") {
			if line != "" {
				flows = append(flows, line)
			}
		}
	}
	return flows
}

// findOfctl returns the recorded ovs-ofctl calls (wrapper prefix stripped),
// which is what the exec-count and ordering assertions compare against.
func (r *ovsRecorder) findOfctl() [][]string {
	var calls [][]string
	for _, c := range r.calls {
		if a := ofctlArgs(c); a != nil {
			calls = append(calls, a)
		}
	}
	return calls
}

// findStrictDeletes returns the match strings of "ovs-ofctl --strict del-flows
// <br> <match>" calls.
func (r *ovsRecorder) findStrictDeletes() []string {
	var matches []string
	for _, c := range r.calls {
		if a := ofctlArgs(c); len(a) >= 5 && a[1] == "--strict" && a[2] == "del-flows" {
			matches = append(matches, a[4])
		}
	}
	return matches
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

// TestParseFlowDump pins the dump spellings the differential apply has to
// read back. The tolerances mirror the ones the integration suite and the E2E
// scenario already observed empirically: nw_dst= (classic) or ip_dst= (OXM),
// the host mask elided or spelled out, ",ip,"/",ipv6," as the protocol
// keyword, IN_PORT for output:in_port, and set_field:…->eth_src/eth_dst for
// the MAC rewrites (test/integration/scenario_fip_test.go,
// test/e2e/scenarios/hairpin.sh).
func TestParseFlowDump(t *testing.T) {
	const (
		hairpinActions = "mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port"
		tweakActions   = "mod_dl_dst:aa:bb:cc:dd:ee:ff,normal"
	)
	tests := []struct {
		name string
		dump string
		want []parsedFlow
	}{
		{
			name: "reply header carries no flow",
			dump: " NXST_FLOW reply (xid=0x4):\n",
		},
		{
			name: "v4 hairpin flow with nw_dst and elided mask",
			dump: " cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n",
			want: []parsedFlow{{
				key:     flowKey{priority: 910, inPort: "42", dst: "5.182.234.199"},
				actions: hairpinActions,
			}},
		},
		{
			name: "v4 hairpin flow with ip_dst and explicit /32",
			dump: " cookie=0x998, table=0, priority=910,ip,in_port=42,ip_dst=5.182.234.199/32 actions=mod_dl_src:AA:BB:CC:DD:EE:FF,mod_dl_dst:FA:16:3E:6F:A1:64,output:in_port\n",
			want: []parsedFlow{{
				key:     flowKey{priority: 910, inPort: "42", dst: "5.182.234.199"},
				actions: hairpinActions,
			}},
		},
		{
			name: "v6 hairpin flow, OXM action spellings and an expanded address",
			dump: " cookie=0x998, table=0, priority=910,ipv6,in_port=7,ipv6_dst=2001:0db8:0000:0000:0000:0000:0000:0001/128 actions=set_field:aa:bb:cc:dd:ee:ff->eth_src,set_field:fa:16:3e:6f:a1:64->eth_dst,IN_PORT\n",
			want: []parsedFlow{{
				key:     flowKey{priority: 910, ipv6: true, inPort: "7", dst: "2001:db8::1"},
				actions: hairpinActions,
			}},
		},
		{
			name: "MAC-tweak pair has no destination",
			dump: " cookie=0x999, table=0, priority=900,ip,in_port=42 actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n" +
				" cookie=0x999, table=0, priority=900,ipv6,in_port=42 actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n",
			want: []parsedFlow{
				{key: flowKey{priority: 900, inPort: "42"}, actions: tweakActions},
				{key: flowKey{priority: 900, ipv6: true, inPort: "42"}, actions: tweakActions},
			},
		},
		{
			name: "stats fields are ignored",
			dump: " cookie=0x999, duration=1234.567s, table=0, n_packets=17, n_bytes=1462, idle_age=3, priority=900,ip,in_port=42 actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n",
			want: []parsedFlow{{key: flowKey{priority: 900, inPort: "42"}, actions: tweakActions}},
		},
		{
			name: "line without a priority is dropped",
			dump: " cookie=0x999, table=0, ip,in_port=42 actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n",
		},
		{
			name: "line with a named in_port is dropped",
			dump: " cookie=0x999, table=0, priority=900,ip,in_port=patch-provnet-0 actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n",
		},
		{
			name: "line without a protocol keyword is dropped",
			dump: " cookie=0x999, table=0, priority=900,in_port=42 actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n",
		},
		{
			name: "line without actions is dropped",
			dump: " cookie=0x999, table=0, priority=900,ip,in_port=42\n",
		},
		{
			name: "line with an unparseable destination is dropped",
			dump: " cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=not-an-ip actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseFlowDump([]byte(tt.dump))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseFlowDump() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParseFlowDumpMatchesDesiredKeys is the property the diff rests on: a
// flow the agent renders and then reads back out of a dump must produce the
// same key and the same normalized actions, or every cycle would delete and
// re-add it.
func TestParseFlowDumpMatchesDesiredKeys(t *testing.T) {
	hairpin := HairpinFlow(ovsCookieHairpin, "42", "2001:DB8:0:0::1", "aa:bb:cc:dd:ee:ff", "FA:16:3E:6F:A1:64", true)
	tweak := MACTweakFlow(ovsCookieMACTweak, "42", "aa:bb:cc:dd:ee:ff", false)

	dump := " NXST_FLOW reply (xid=0x4):\n" +
		" cookie=0x998, table=0, priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1 actions=set_field:aa:bb:cc:dd:ee:ff->eth_src,set_field:fa:16:3e:6f:a1:64->eth_dst,IN_PORT\n" +
		" cookie=0x999, table=0, priority=900,ip,in_port=42 actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n"

	want := []parsedFlow{
		{
			key:     flowKey{priority: hairpinFlowPriority, ipv6: true, inPort: "42", dst: "2001:db8::1"},
			actions: normalizeActions(flowActions(hairpin)),
		},
		{
			key:     flowKey{priority: macTweakFlowPriority, inPort: "42"},
			actions: normalizeActions(flowActions(tweak)),
		},
	}
	if got := parseFlowDump([]byte(dump)); !reflect.DeepEqual(got, want) {
		t.Errorf("parseFlowDump() = %+v, want %+v", got, want)
	}
}

func TestNormalizeActions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"IN_PORT is the dumped form of output:in_port", "IN_PORT", "output:in_port"},
		{"output:in_port is left alone", "output:in_port", "output:in_port"},
		{"OXM eth_src write", "set_field:AA:BB:CC:DD:EE:FF->eth_src", "mod_dl_src:aa:bb:cc:dd:ee:ff"},
		{"OXM eth_dst write", "set_field:fa:16:3e:6f:a1:64->eth_dst", "mod_dl_dst:fa:16:3e:6f:a1:64"},
		{"mixed-case MAC lowercased", "mod_dl_dst:FA:16:3E:6F:A1:64,NORMAL", "mod_dl_dst:fa:16:3e:6f:a1:64,normal"},
		{
			"full hairpin action list in dumped spelling",
			"set_field:aa:bb:cc:dd:ee:ff->eth_src,set_field:FA:16:3E:6F:A1:64->eth_dst,IN_PORT",
			"mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeActions(tt.in); got != tt.want {
				t.Errorf("normalizeActions(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFlowKeyDeleteMatch(t *testing.T) {
	tests := []struct {
		name   string
		key    flowKey
		cookie string
		want   string
	}{
		{
			"v4 hairpin flow",
			flowKey{priority: 910, inPort: "42", dst: "5.182.234.199"},
			ovsCookieHairpin,
			"cookie=0x998/-1,priority=910,ip,in_port=42,nw_dst=5.182.234.199",
		},
		{
			"v6 hairpin flow",
			flowKey{priority: 910, ipv6: true, inPort: "7", dst: "2001:db8::1"},
			ovsCookieHairpin,
			"cookie=0x998/-1,priority=910,ipv6,in_port=7,ipv6_dst=2001:db8::1",
		},
		{
			"v4 MAC-tweak flow has no destination",
			flowKey{priority: 900, inPort: "42"},
			ovsCookieMACTweak,
			"cookie=0x999/-1,priority=900,ip,in_port=42",
		},
		{
			"v6 MAC-tweak flow has no destination",
			flowKey{priority: 900, ipv6: true, inPort: "42"},
			ovsCookieMACTweak,
			"cookie=0x999/-1,priority=900,ipv6,in_port=42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.deleteMatch(tt.cookie); got != tt.want {
				t.Errorf("deleteMatch() = %q, want %q", got, tt.want)
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
// resolves to its ofport and an empty plane, EnsureSegments validates the
// ofport, dumps the plane once, probes for bundle support, and installs the
// IPv4+IPv6 MAC-tweak pair in one batched exec. Nothing is ever deleted.
func TestEnsureSegmentsWithCachedFallbackKeepsFlatFlowSet(t *testing.T) {
	rec := newOVSRecorder()
	// EnsureSegments re-validates every cached binding on each call; here
	// the patch port still resolves to ofport 42, so no rediscovery runs.
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"42\n", nil,
	)
	rec.onDump("br-ex", ovsCookieMACTweak, " NXST_FLOW reply (xid=0x4):\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}}); err != nil {
		t.Fatalf("EnsureSegments() error: %v", err)
	}

	// Expect: 1 ofport validation + dump + bundle probe + batched add.
	if len(rec.calls) != 4 {
		t.Fatalf("expected 4 OVS commands, got %d: %v", len(rec.calls), rec.calls)
	}

	wantValidate := []string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"}
	if !reflect.DeepEqual(rec.calls[0], wantValidate) {
		t.Errorf("first call = %v, want %v", rec.calls[0], wantValidate)
	}
	wantOfctl := [][]string{
		{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x999/-1"},
		{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"},
		{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"},
	}
	if got := rec.findOfctl(); !reflect.DeepEqual(got, wantOfctl) {
		t.Errorf("ovs-ofctl calls = %v, want %v", got, wantOfctl)
	}

	flows := rec.findBatchedFlows()
	wantFlows := []string{
		"cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL",
	}
	if !reflect.DeepEqual(flows, wantFlows) {
		t.Errorf("batched flows = %v, want %v", flows, wantFlows)
	}
}

// TestEnsureSegmentsUnchangedPlaneIsReadOnly is the headline property of the
// differential apply on the MAC-tweak plane: when the installed flows already
// match the desired set, the reconcile issues the dump and nothing else. The
// dump is spelled the way OVS renders it, including the IN_PORT-era
// canonicalizations, so the comparison exercises the normalizer too.
func TestEnsureSegmentsUnchangedPlaneIsReadOnly(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"42\n", nil,
	)
	rec.onDump("br-ex", ovsCookieMACTweak,
		" NXST_FLOW reply (xid=0x4):\n"+
			" cookie=0x999, table=0, priority=900,ip,in_port=42 actions=mod_dl_dst:AA:BB:CC:DD:EE:FF,NORMAL\n"+
			" cookie=0x999, table=0, priority=900,ipv6,in_port=42 actions=set_field:aa:bb:cc:dd:ee:ff->eth_dst,NORMAL\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}}); err != nil {
		t.Fatalf("EnsureSegments() error: %v", err)
	}

	wantOfctl := [][]string{{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x999/-1"}}
	if got := rec.findOfctl(); !reflect.DeepEqual(got, wantOfctl) {
		t.Errorf("steady-state cycle must issue only the dump, got %v", got)
	}
}

// TestEnsureSegmentsInstallsFlowsPerPatchPort covers the multi-VLAN path:
// two localnet segments resolve to their own patch ports (via the
// external_ids ovn-controller stamps on the Port rows) and their own kernel
// interfaces (via the segment-interface hook), and the MAC-tweak flows are
// installed per patch port with the per-segment MACs, in one batch and
// without deleting anything.
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

	flows := rec.findBatchedFlows()
	wantFlows := []string{
		"cookie=0x999,priority=900,ip,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL",
		"cookie=0x999,priority=900,ip,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
	}
	if !reflect.DeepEqual(flows, wantFlows) {
		t.Errorf("batched flows = %v, want %v", flows, wantFlows)
	}

	// Nothing is deleted: the plane was empty, so the diff is adds only.
	for _, c := range rec.calls {
		if a := ofctlArgs(c); a != nil && strings.Contains(strings.Join(a, " "), "del-flows") {
			t.Errorf("unexpected delete on an empty plane: %v", a)
		}
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

	flows := rec.findBatchedFlows()
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

// TestEnsureSegmentsDumpFailureIsSurfaced covers the read that opens the
// apply: without a dump there is no diff, so the reconcile reports the
// wrapped error and touches nothing. The previously installed flows keep
// forwarding in the meantime.
func TestEnsureSegmentsDumpFailureIsSurfaced(t *testing.T) {
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"42\n", nil,
	)
	rec.on(
		[]string{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x999/-1"},
		"some output", errors.New("transient ofctl error"),
	)
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}})
	if err == nil || !strings.Contains(err.Error(), "dump cookie=0x999 flows on br-ex") {
		t.Fatalf("expected the wrapped dump error, got: %v", err)
	}
	if got := rec.findOfctl(); len(got) != 1 {
		t.Errorf("a failed dump must mutate nothing, got %v", got)
	}
}

// TestEnsureSegmentsBatchedAddFailureIsSurfaced covers the failure of the
// batched add on the MAC-tweak plane: the bundle applied nothing, so the
// previously installed flows are still forwarding, and the error reaches the
// caller instead of being swallowed the way the per-flow re-adds were.
func TestEnsureSegmentsBatchedAddFailureIsSurfaced(t *testing.T) {
	m := withTestMetrics(t)
	rec := newOVSRecorder()
	rec.on(
		[]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"},
		"42\n", nil,
	)
	rec.onDump("br-ex", ovsCookieMACTweak, " NXST_FLOW reply (xid=0x4):\n")
	batch := "cookie=0x999,priority=900,ip,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n" +
		"cookie=0x999,priority=900,ipv6,in_port=42,actions=mod_dl_dst:aa:bb:cc:dd:ee:ff,NORMAL\n"
	rec.onStdin([]string{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"}, batch,
		"bundle failed", errors.New("ofctl exit 1"))
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}})
	if err == nil || !strings.Contains(err.Error(), "ofctl exit 1") {
		t.Fatalf("expected the batched add failure to surface, got: %v", err)
	}
	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "mactweak"); v != 1 {
		t.Errorf("ovs_flow_apply_errors_total{plane=\"mactweak\"} = %v, want 1 for the failed batch", v)
	}
}

// TestEnsureSegmentsAddFailureDoesNotStarveOthers verifies the per-flow
// fallback mode, taken when the configured wrapper does not forward stdin
// (`docker exec` without -i: the probe's `cat` echoes nothing back). Every
// flow gets its own add-flow exec, a failure on one does not stop the rest,
// and the collected failures are returned naming the flow that broke — never
// a silent skip.
func TestEnsureSegmentsAddFailureDoesNotStarveOthers(t *testing.T) {
	m := withTestMetrics(t)
	rec := newOVSRecorder()
	rec.on(
		[]string{"docker", "exec", "ovs", "ovs-vsctl", "list-ports", "br-ex"},
		"patch-seg101\npatch-seg102\n", nil,
	)
	rec.on(
		[]string{"docker", "exec", "ovs", "ovs-vsctl", "--if-exists", "get", "Port", "patch-seg101", "external_ids:ovn-localnet-port"},
		"\"seg-101\"\n", nil,
	)
	rec.on(
		[]string{"docker", "exec", "ovs", "ovs-vsctl", "--if-exists", "get", "Port", "patch-seg102", "external_ids:ovn-localnet-port"},
		"\"seg-102\"\n", nil,
	)
	rec.on(
		[]string{"docker", "exec", "ovs", "ovs-vsctl", "get", "Interface", "patch-seg101", "ofport"},
		"5\n", nil,
	)
	rec.on(
		[]string{"docker", "exec", "ovs", "ovs-vsctl", "get", "Interface", "patch-seg102", "ofport"},
		"6\n", nil,
	)
	// The wrapper swallows stdin: `cat` sees EOF, prints nothing, exits 0.
	rec.on([]string{"docker", "exec", "ovs", "cat"}, "", nil)
	// The first segment's IPv4 add-flow fails.
	rec.on(
		[]string{"docker", "exec", "ovs", "ovs-ofctl", "add-flow", "br-ex",
			"cookie=0x999,priority=900,ip,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL"},
		"add-flow failed", errors.New("bad flow"),
	)
	rm := &RouteManager{
		cfg:         Config{BridgeDev: "br-ex", OVSWrapper: "docker exec ovs"},
		ovsWrapper:  []string{"docker", "exec", "ovs"},
		execOVSHook: rec.hook(),
		segmentIfaceHook: func(tag int) (string, string, error) {
			return fmt.Sprintf("br-ex.%d", tag), fmt.Sprintf("aa:bb:cc:dd:ee:%02x", tag), nil
		},
	}

	tag101, tag102 := 101, 102
	desired := []DesiredSegment{
		{LocalnetPort: "seg-101", VLANTag: &tag101},
		{LocalnetPort: "seg-102", VLANTag: &tag102},
	}
	err := rm.EnsureSegments(desired)
	if err == nil || !strings.Contains(err.Error(), "bad flow") {
		t.Fatalf("expected the failed flow to be reported, got: %v", err)
	}
	if !strings.Contains(err.Error(), "in_port=5") {
		t.Errorf("error must name the failed flow, got: %v", err)
	}

	// Every flow is attempted, including the healthy segment's pair behind
	// the failure.
	wantAttempted := []string{
		"cookie=0x999,priority=900,ip,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL",
		"cookie=0x999,priority=900,ip,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=5,actions=mod_dl_dst:aa:bb:cc:dd:ee:65,NORMAL",
		"cookie=0x999,priority=900,ipv6,in_port=6,actions=mod_dl_dst:aa:bb:cc:dd:ee:66,NORMAL",
	}
	if got := rec.findAddFlows(); !reflect.DeepEqual(got, wantAttempted) {
		t.Errorf("per-flow adds = %v, want every flow attempted: %v", got, wantAttempted)
	}
	if got := rec.findBatchedFlows(); len(got) != 0 {
		t.Errorf("no batch may be piped into a wrapper that eats stdin, got %v", got)
	}
	// One failed add, one counted mutation — the three that succeeded are
	// not errors.
	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "mactweak"); v != 1 {
		t.Errorf("ovs_flow_apply_errors_total{plane=\"mactweak\"} = %v, want 1", v)
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
	rec.onDump("br-ex", ovsCookieHairpin, " NXST_FLOW reply (xid=0x4):\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
		"2001:db8::1":   {RouterMAC: "fa:16:3e:00:00:01"},
	}

	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	// The plane is read before it is written, and nothing is deleted.
	wantOfctl := [][]string{
		{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"},
		{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"},
		{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"},
	}
	if got := rec.findOfctl(); !reflect.DeepEqual(got, wantOfctl) {
		t.Errorf("ovs-ofctl calls = %v, want %v", got, wantOfctl)
	}

	// One spec per IP, batched through stdin in sorted order — map
	// iteration order must not reach ovs-ofctl.
	got := rec.findBatchedFlows()
	want := []string{
		"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port",
		"cookie=0x998,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1/128,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,output:in_port",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("batched flows = %v, want %v", got, want)
	}
}

// TestReconcileOVSHairpinFlowsUnchangedPlaneIsReadOnly is the headline
// property of this change: a cycle whose desired set already matches the
// installed flows issues the dump and nothing else, so the flows — and their
// packet counters — are never interrupted. The dump is spelled the way OVS
// renders it back (nw_dst without the /32, IN_PORT for output:in_port).
func TestReconcileOVSHairpinFlowsUnchangedPlaneIsReadOnly(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" NXST_FLOW reply (xid=0x4):\n"+
			" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n"+
			" cookie=0x998, table=0, priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
		"2001:db8::1":   {RouterMAC: "FA:16:3E:00:00:01"},
	}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	wantOfctl := [][]string{{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"}}
	if got := rec.findOfctl(); !reflect.DeepEqual(got, wantOfctl) {
		t.Errorf("steady-state cycle must issue only the dump, got %v", got)
	}
}

// TestReconcileOVSHairpinFlowsChangedActionsReplaceInPlace covers a FIP that
// moved to another router: same match, new mod_dl_dst. The flow is re-added
// under the identical match and priority — which OpenFlow replaces atomically
// — and is never deleted first, so the destination is reachable throughout.
func TestReconcileOVSHairpinFlowsChangedActionsReplaceInPlace(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:aa,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	want := []string{"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port"}
	if got := rec.findBatchedFlows(); !reflect.DeepEqual(got, want) {
		t.Errorf("batched flows = %v, want the re-add %v", got, want)
	}
	if got := rec.findStrictDeletes(); len(got) != 0 {
		t.Errorf("a changed flow must be replaced, not deleted first; got deletes %v", got)
	}
}

// TestReconcileOVSHairpinFlowsDeletesStaleAfterAdds pins the order the whole
// change rests on: the new flow is installed before the retired one is
// removed, and the removal is strict and fully qualified so it can only match
// the one entry.
func TestReconcileOVSHairpinFlowsDeletesStaleAfterAdds(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=203.0.113.50 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:01:02,IN_PORT\n"+
			" cookie=0x998, table=0, priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::99 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:99,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	// The v4 FIP is gone, a new one took its place; the v6 one is gone too.
	targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	wantOfctl := [][]string{
		{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"},
		{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"},
		{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"},
		{"ovs-ofctl", "--strict", "del-flows", "br-ex", "cookie=0x998/-1,priority=910,ip,in_port=42,nw_dst=203.0.113.50"},
		{"ovs-ofctl", "--strict", "del-flows", "br-ex", "cookie=0x998/-1,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::99"},
	}
	if got := rec.findOfctl(); !reflect.DeepEqual(got, wantOfctl) {
		t.Errorf("ovs-ofctl calls = %v, want adds before deletes: %v", got, wantOfctl)
	}
}

// TestReconcileOVSHairpinFlowsIgnoresUnparseableDumpLine proves a dump line
// the agent cannot key is dropped rather than guessed at: it produces no
// delete, and the desired flow it partly resembles is simply (re-)added.
func TestReconcileOVSHairpinFlowsIgnoresUnparseableDumpLine(t *testing.T) {
	logs := captureSlog(t)
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=patch-provnet-0,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	if got := rec.findStrictDeletes(); len(got) != 0 {
		t.Errorf("an unkeyable dump line must not synthesize a delete, got %v", got)
	}
	if got := len(rec.findBatchedFlows()); got != 1 {
		t.Errorf("expected the desired flow to be (re-)added, got %d batched flows", got)
	}
	if !strings.Contains(logs.String(), "ignoring unparseable OVS flow dump line") {
		t.Errorf("expected a warning about the dropped line; log:\n%s", logs.String())
	}
}

// TestReconcileOVSHairpinFlowsDeleteFailureDoesNotStopOthers verifies that one
// failing strict delete neither hides the remaining stale flows nor the error
// itself.
func TestReconcileOVSHairpinFlowsDeleteFailureDoesNotStopOthers(t *testing.T) {
	m := withTestMetrics(t)
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=203.0.113.50 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:01:02,IN_PORT\n"+
			" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=203.0.113.51 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:01:03,IN_PORT\n")
	rec.on([]string{"ovs-ofctl", "--strict", "del-flows", "br-ex",
		"cookie=0x998/-1,priority=910,ip,in_port=42,nw_dst=203.0.113.50"},
		"del failed", errors.New("ofctl exit 1"))
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	err := rm.ReconcileOVSHairpinFlows(nil)
	if err == nil || !strings.Contains(err.Error(), "ofctl exit 1") {
		t.Fatalf("expected the failed delete to be reported, got: %v", err)
	}
	want := []string{
		"cookie=0x998/-1,priority=910,ip,in_port=42,nw_dst=203.0.113.50",
		"cookie=0x998/-1,priority=910,ip,in_port=42,nw_dst=203.0.113.51",
	}
	if got := rec.findStrictDeletes(); !reflect.DeepEqual(got, want) {
		t.Errorf("strict deletes = %v, want both attempted: %v", got, want)
	}
	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "hairpin"); v != 1 {
		t.Errorf("ovs_flow_apply_errors_total{plane=\"hairpin\"} = %v, want 1 for the failed sweep", v)
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

	got := rec.findBatchedFlows()
	want := []string{
		"cookie=0x998,priority=910,ip,in_port=5,ip_dst=198.51.100.50/32,actions=mod_dl_src:aa:bb:cc:dd:ee:65,mod_dl_dst:fa:16:3e:00:01:01,output:in_port",
		"cookie=0x998,priority=910,ip,in_port=6,ip_dst=203.0.113.50/32,actions=mod_dl_src:aa:bb:cc:dd:ee:66,mod_dl_dst:fa:16:3e:00:01:02,output:in_port",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("batched flows = %v, want %v", got, want)
	}
}

// TestReconcileOVSHairpinFlowsEmptyMapClearsAll covers the plane going empty
// (no locally-active routers left): every installed flow is retired with its
// own strict delete, and nothing is added.
func TestReconcileOVSHairpinFlowsEmptyMapClearsAll(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n"+
			" cookie=0x998, table=0, priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:00:01,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(nil); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows(nil) error: %v", err)
	}

	wantOfctl := [][]string{
		{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"},
		{"ovs-ofctl", "--strict", "del-flows", "br-ex", "cookie=0x998/-1,priority=910,ip,in_port=42,nw_dst=5.182.234.199"},
		{"ovs-ofctl", "--strict", "del-flows", "br-ex", "cookie=0x998/-1,priority=910,ipv6,in_port=42,ipv6_dst=2001:db8::1"},
	}
	if got := rec.findOfctl(); !reflect.DeepEqual(got, wantOfctl) {
		t.Errorf("ovs-ofctl calls = %v, want %v", got, wantOfctl)
	}
}

// TestReconcileOVSHairpinFlowsEmptyMapEmptyPlaneOnlyDumps covers the other
// empty case: nothing desired and nothing installed costs exactly one read.
func TestReconcileOVSHairpinFlowsEmptyMapEmptyPlaneOnlyDumps(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin, " NXST_FLOW reply (xid=0x4):\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(nil); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows(nil) error: %v", err)
	}

	wantOfctl := [][]string{{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"}}
	if got := rec.findOfctl(); !reflect.DeepEqual(got, wantOfctl) {
		t.Errorf("ovs-ofctl calls = %v, want only the dump", got)
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

// TestReconcileOVSHairpinFlowsSkipsInvalidIP proves a row the agent cannot
// render — an unparseable IP, or a segment with no binding — is left out of
// the desired set with a warning and does not prevent the healthy flow from
// installing (issue #158 test b, hairpin half). Whatever stale flow such a row
// left behind is retired like any other flow that is no longer wanted.
func TestReconcileOVSHairpinFlowsSkipsInvalidIP(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=192.0.2.7 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:aa:aa:aa:aa:aa:aa,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"not-an-ip":     {RouterMAC: "aa:aa:aa:aa:aa:aa"},
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
	}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() must skip the invalid IP, got: %v", err)
	}

	wantHealthy := []string{"cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port"}
	if got := rec.findBatchedFlows(); !reflect.DeepEqual(got, wantHealthy) {
		t.Errorf("batched flows = %v, want only the healthy one: %v", got, wantHealthy)
	}
	wantDeletes := []string{"cookie=0x998/-1,priority=910,ip,in_port=42,nw_dst=192.0.2.7"}
	if got := rec.findStrictDeletes(); !reflect.DeepEqual(got, wantDeletes) {
		t.Errorf("strict deletes = %v, want the stale flow retired: %v", got, wantDeletes)
	}
}

// TestReconcileOVSHairpinFlowsBatchedAddFailureSkipsDeletes proves a failed
// batch stops the apply before the delete phase: the bundle installed nothing,
// so shrinking the plane on top of that would take out flows that are still
// the only ones forwarding. The error reaches the caller.
func TestReconcileOVSHairpinFlowsBatchedAddFailureSkipsDeletes(t *testing.T) {
	m := withTestMetrics(t)
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=203.0.113.50 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:01:02,IN_PORT\n")
	batch := "cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port\n"
	rec.onStdin([]string{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"}, batch,
		"bundle failed", errors.New("bad flow"))
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}}
	err := rm.ReconcileOVSHairpinFlows(targets)
	if err == nil || !strings.Contains(err.Error(), "bad flow") {
		t.Fatalf("expected the batch failure to surface, got: %v", err)
	}
	if got := rec.findStrictDeletes(); len(got) != 0 {
		t.Errorf("a failed grow must not be followed by a shrink, got deletes %v", got)
	}
	if rm.ofctlBundleOK != nil {
		t.Error("a bundle-mode failure must drop the cached verdict so the next cycle re-probes")
	}
	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "hairpin"); v != 1 {
		t.Errorf("ovs_flow_apply_errors_total{plane=\"hairpin\"} = %v, want 1 for the failed batch", v)
	}
}

// TestReconcileOVSHairpinFlowsPerFlowFallbackJoinsFailures covers the same
// failure in the per-flow mode a stdin-less wrapper forces: every remaining
// flow is still attempted and the failures come back together, each naming its
// own flow.
func TestReconcileOVSHairpinFlowsPerFlowFallbackJoinsFailures(t *testing.T) {
	m := withTestMetrics(t)
	rec := newOVSRecorder()
	rec.on([]string{"docker", "exec", "ovs", "ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"},
		" NXST_FLOW reply (xid=0x4):\n", nil)
	// The wrapper swallows stdin, so `cat` echoes nothing back.
	rec.on([]string{"docker", "exec", "ovs", "cat"}, "", nil)
	const failingFlow = "cookie=0x998,priority=910,ip,in_port=42,ip_dst=203.0.113.50/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:00:01:02,output:in_port"
	rec.on([]string{"docker", "exec", "ovs", "ovs-ofctl", "add-flow", "br-ex", failingFlow},
		"add-flow failed", errors.New("bad flow"))
	rm := &RouteManager{
		cfg:         Config{BridgeDev: "br-ex", OVSWrapper: "docker exec ovs"},
		ovsWrapper:  []string{"docker", "exec", "ovs"},
		segments:    fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"),
		execOVSHook: rec.hook(),
	}

	targets := map[string]HairpinTarget{
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
		"203.0.113.50":  {RouterMAC: "fa:16:3e:00:01:02"},
	}
	err := rm.ReconcileOVSHairpinFlows(targets)
	if err == nil || !strings.Contains(err.Error(), "ip_dst=203.0.113.50/32") {
		t.Fatalf("expected the failed flow to be named in the error, got: %v", err)
	}

	wantSurviving := "cookie=0x998,priority=910,ip,in_port=42,ip_dst=5.182.234.199/32,actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,output:in_port"
	if !containsFlow(rec.findAddFlows(), wantSurviving) {
		t.Errorf("healthy flow not installed after the sibling's add-flow failure; got %v", rec.findAddFlows())
	}
	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "hairpin"); v != 1 {
		t.Errorf("ovs_flow_apply_errors_total{plane=\"hairpin\"} = %v, want 1 for the one failed add", v)
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

// hairpinPlaneGauges reads the pair back as (desired, installed), which is
// how every assertion below wants them.
func hairpinPlaneGauges(t *testing.T, m *metricsRegistry) (float64, float64) {
	t.Helper()
	return gaugeValue(t, m, "ovn_network_agent_hairpin_flows_desired"),
		gaugeValue(t, m, "ovn_network_agent_hairpin_flows_installed")
}

// TestReconcileOVSHairpinFlowsReportsThePlaneItFound pins the observation
// point: `installed` is the pre-apply dump, not a post-apply count. A flow
// deleted out from under the agent therefore shows as a deficit for the cycle
// that heals it — which is the only window the alert can fire in.
func TestReconcileOVSHairpinFlowsReportsThePlaneItFound(t *testing.T) {
	m := withTestMetrics(t)
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{
		"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"},
		"203.0.113.50":  {RouterMAC: "fa:16:3e:00:01:02"},
	}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}

	desired, installed := hairpinPlaneGauges(t, m)
	if desired != 2 || installed != 1 {
		t.Errorf("hairpin plane = %v desired / %v installed, want 2/1 from the pre-apply dump", desired, installed)
	}
}

// Nil targets want nothing; the plane still reports what the dump found, and
// the cycle after the sweep reports the cleared plane as 0/0.
func TestReconcileOVSHairpinFlowsEmptyTargetsReportZeroDesired(t *testing.T) {
	m := withTestMetrics(t)
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin,
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(nil); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows(nil) error: %v", err)
	}
	if desired, installed := hairpinPlaneGauges(t, m); desired != 0 || installed != 1 {
		t.Errorf("hairpin plane = %v/%v after the sweep cycle, want 0 desired / 1 installed", desired, installed)
	}

	// Next cycle: the plane the sweep emptied.
	rec.onDump("br-ex", ovsCookieHairpin, " NXST_FLOW reply (xid=0x4):\n")
	if err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{}); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows(empty) error: %v", err)
	}
	if desired, installed := hairpinPlaneGauges(t, m); desired != 0 || installed != 0 {
		t.Errorf("hairpin plane = %v/%v on the cleared plane, want 0/0", desired, installed)
	}
}

// A just-started agent has no segment bindings yet and touches no flows, so
// it must not publish a 0/N deficit the alert would fire on.
func TestReconcileOVSHairpinFlowsNoBindingsLeavesGaugesUntouched(t *testing.T) {
	m := withTestMetrics(t)
	setHairpinFlowPlane(3, 3)
	rec := newOVSRecorder()
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{"10.0.0.1": {RouterMAC: "aa:aa:aa:aa:aa:aa"}}); err != nil {
		t.Fatalf("expected a no-op, got: %v", err)
	}
	if desired, installed := hairpinPlaneGauges(t, m); desired != 3 || installed != 3 {
		t.Errorf("hairpin plane = %v/%v, want the previous 3/3 left alone", desired, installed)
	}
}

// The dump is the differential apply's own input, so a failed dump aborts the
// cycle (issue #241's behaviour, unchanged here). What this pins is that the
// observation does not lie about it: neither gauge moves, so the last good
// reading stands rather than a fabricated 0.
func TestReconcileOVSHairpinFlowsDumpFailureLeavesGaugesUntouched(t *testing.T) {
	m := withTestMetrics(t)
	setHairpinFlowPlane(4, 4)
	rec := newOVSRecorder()
	rec.on([]string{"ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"},
		"some output", errors.New("transient ofctl error"))
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}})
	if err == nil || !strings.Contains(err.Error(), "dump cookie=0x998 flows on br-ex") {
		t.Fatalf("expected the wrapped dump error, got: %v", err)
	}
	if desired, installed := hairpinPlaneGauges(t, m); desired != 4 || installed != 4 {
		t.Errorf("hairpin plane = %v/%v after a failed dump, want the previous 4/4", desired, installed)
	}
	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "hairpin"); v != 0 {
		t.Errorf("a failed dump is not a failed mutation, got %v apply errors", v)
	}
}

// Dry-run reconciles nothing, so it must observe nothing either.
func TestReconcileOVSHairpinFlowsDryRunLeavesGaugesUntouched(t *testing.T) {
	m := withTestMetrics(t)
	setHairpinFlowPlane(2, 2)
	rec := newOVSRecorder()
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex", DryRun: true}, execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{"10.0.0.1": {RouterMAC: "aa:aa:aa:aa:aa:aa"}}); err != nil {
		t.Fatalf("dry-run should not error: %v", err)
	}
	if desired, installed := hairpinPlaneGauges(t, m); desired != 2 || installed != 2 {
		t.Errorf("hairpin plane = %v/%v in dry-run, want the previous 2/2", desired, installed)
	}
}

// The MAC-tweak plane shares reconcileFlowPlane but has no gauges of its own,
// so its cycles must never move the hairpin pair.
func TestEnsureSegmentsLeavesTheHairpinGaugesAlone(t *testing.T) {
	m := withTestMetrics(t)
	setHairpinFlowPlane(5, 5)
	rec := newOVSRecorder()
	rec.on([]string{"ovs-vsctl", "get", "Interface", "patch-provnet-0", "ofport"}, "42\n", nil)
	rec.onDump("br-ex", ovsCookieMACTweak, " NXST_FLOW reply (xid=0x4):\n")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.EnsureSegments([]DesiredSegment{{LocalnetPort: ""}}); err != nil {
		t.Fatalf("EnsureSegments() error: %v", err)
	}
	if desired, installed := hairpinPlaneGauges(t, m); desired != 5 || installed != 5 {
		t.Errorf("hairpin plane = %v/%v after a MAC-tweak reconcile, want the previous 5/5", desired, installed)
	}
}

// wrapperRM returns a RouteManager wired to the given recorder with a
// containerized OVS wrapper configured, which is what makes the stdin probe
// run at all.
func wrapperRM(rec *ovsRecorder) *RouteManager {
	return &RouteManager{
		cfg:         Config{BridgeDev: "br-ex", OVSWrapper: "docker exec ovs"},
		ovsWrapper:  []string{"docker", "exec", "ovs"},
		segments:    fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"),
		execOVSHook: rec.hook(),
	}
}

// countCalls returns how many recorded calls contain the given joined
// substring.
func (r *ovsRecorder) countCalls(substr string) int {
	n := 0
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			n++
		}
	}
	return n
}

// TestStdinProbeSkippedWithoutWrapper: with ovs-ofctl on PATH there is no
// wrapper that could eat stdin, so no probe is worth an exec.
func TestStdinProbeSkippedWithoutWrapper(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin, "")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	if err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}}); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}
	if n := rec.countCalls("cat"); n != 0 {
		t.Errorf("expected no stdin probe without a wrapper, got %d", n)
	}
	if rm.wrapperStdinOK != nil {
		t.Error("no wrapper means no verdict to cache")
	}
}

// TestProbesOnlyRunOnANonEmptyDiff: a steady-state cycle applies nothing, so
// neither probe has anything to decide.
func TestProbesOnlyRunOnANonEmptyDiff(t *testing.T) {
	rec := newOVSRecorder()
	rec.on([]string{"docker", "exec", "ovs", "ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"},
		" cookie=0x998, table=0, priority=910,ip,in_port=42,nw_dst=5.182.234.199 actions=mod_dl_src:aa:bb:cc:dd:ee:ff,mod_dl_dst:fa:16:3e:6f:a1:64,IN_PORT\n", nil)
	rm := wrapperRM(rec)

	if err := rm.ReconcileOVSHairpinFlows(map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}}); err != nil {
		t.Fatalf("ReconcileOVSHairpinFlows() error: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Errorf("expected the dump alone, got %v", rec.calls)
	}
	if rm.wrapperStdinOK != nil || rm.ofctlBundleOK != nil {
		t.Error("probes must not run on a cycle that applies nothing")
	}
}

// TestStdinProbeCachesEchoedVerdict: a wrapper that echoes the probe payload
// back forwards stdin, and the verdict is cached — the second apply pipes its
// batch without probing again.
func TestStdinProbeCachesEchoedVerdict(t *testing.T) {
	rec := newOVSRecorder()
	rec.on([]string{"docker", "exec", "ovs", "cat"}, ovsStdinProbe+"\n", nil)
	rec.on([]string{"docker", "exec", "ovs", "ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"}, "", nil)
	rm := wrapperRM(rec)

	for i := 0; i < 2; i++ {
		targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: fmt.Sprintf("fa:16:3e:00:00:%02d", i)}}
		if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	if n := rec.countCalls(" cat"); n != 1 {
		t.Errorf("expected exactly 1 stdin probe across two applies, got %d", n)
	}
	if rm.wrapperStdinOK == nil || !*rm.wrapperStdinOK {
		t.Errorf("expected a cached positive stdin verdict, got %v", rm.wrapperStdinOK)
	}
	if got := len(rec.findBatchedFlows()); got != 2 {
		t.Errorf("expected both cycles to batch their add, got %d batched flows", got)
	}
}

// TestStdinProbeCachesNegativeVerdictAndWarnsOnce covers `docker exec` without
// -i: `cat` reads EOF, prints nothing and exits 0. The apply degrades to one
// exec per flow and says so once, naming the fix.
func TestStdinProbeCachesNegativeVerdictAndWarnsOnce(t *testing.T) {
	logs := captureSlog(t)
	rec := newOVSRecorder()
	rec.on([]string{"docker", "exec", "ovs", "cat"}, "", nil)
	rec.on([]string{"docker", "exec", "ovs", "ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"}, "", nil)
	rm := wrapperRM(rec)

	for i := 0; i < 2; i++ {
		targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: fmt.Sprintf("fa:16:3e:00:00:%02d", i)}}
		if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	if n := rec.countCalls(" cat"); n != 1 {
		t.Errorf("expected the negative verdict to be cached after 1 probe, got %d probes", n)
	}
	if rm.wrapperStdinOK == nil || *rm.wrapperStdinOK {
		t.Errorf("expected a cached negative stdin verdict, got %v", rm.wrapperStdinOK)
	}
	if got := len(rec.findAddFlows()); got != 2 {
		t.Errorf("expected one per-flow add per cycle, got %d", got)
	}
	if n := strings.Count(logs.String(), "does not forward stdin"); n != 1 {
		t.Errorf("expected exactly 1 stdin warning, got %d; log:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "docker exec -i") {
		t.Errorf("the warning must name the fix; log:\n%s", logs.String())
	}
}

// TestStdinProbeCachesNothingOnExecError: a probe that could not run says
// nothing about stdin, so the next apply probes again instead of degrading for
// the rest of the agent's life.
func TestStdinProbeCachesNothingOnExecError(t *testing.T) {
	rec := newOVSRecorder()
	rec.on([]string{"docker", "exec", "ovs", "cat"}, "no such container", errors.New("exit 125"))
	rec.on([]string{"docker", "exec", "ovs", "ovs-ofctl", "--no-stats", "dump-flows", "br-ex", "cookie=0x998/-1"}, "", nil)
	rm := wrapperRM(rec)

	for i := 0; i < 2; i++ {
		targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: fmt.Sprintf("fa:16:3e:00:00:%02d", i)}}
		if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	if rm.wrapperStdinOK != nil {
		t.Errorf("a failed probe must cache no verdict, got %v", *rm.wrapperStdinOK)
	}
	if n := rec.countCalls(" cat"); n != 2 {
		t.Errorf("expected the probe to be retried on the next apply, got %d probes", n)
	}
}

// TestBundleProbeCachesSuccess: the empty-bundle probe runs once and its
// positive verdict is reused.
func TestBundleProbeCachesSuccess(t *testing.T) {
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin, "")
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	for i := 0; i < 2; i++ {
		targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: fmt.Sprintf("fa:16:3e:00:00:%02d", i)}}
		if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}

	if rm.ofctlBundleOK == nil || !*rm.ofctlBundleOK {
		t.Errorf("expected a cached positive bundle verdict, got %v", rm.ofctlBundleOK)
	}
	// 2 dumps + 1 probe + 2 batched adds.
	if len(rec.calls) != 5 {
		t.Errorf("expected the probe to run once across two applies, got %v", rec.calls)
	}
}

// TestBundleProbeNeverCachesFailure mirrors the ovs-ofctl failure-injection
// scenario: every call fails for one cycle. The apply falls back to a plain
// add-flows batch and warns once, and because the negative verdict is not
// cached the next cycle probes again and recovers the atomic path.
func TestBundleProbeNeverCachesFailure(t *testing.T) {
	logs := captureSlog(t)
	rec := newOVSRecorder()
	rec.onDump("br-ex", ovsCookieHairpin, "")
	rec.onStdin([]string{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"}, "",
		"version negotiation failed", errors.New("exit 1"))
	rm := &RouteManager{cfg: Config{BridgeDev: "br-ex"}, segments: fallbackSegments("patch-provnet-0", "42", "aa:bb:cc:dd:ee:ff"), execOVSHook: rec.hook()}

	targets := map[string]HairpinTarget{"5.182.234.199": {RouterMAC: "fa:16:3e:6f:a1:64"}}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("a failed bundle probe must not fail the apply, got: %v", err)
	}
	if rm.ofctlBundleOK != nil {
		t.Errorf("a negative bundle verdict must not be cached, got %v", *rm.ofctlBundleOK)
	}
	wantPlain := []string{"ovs-ofctl", "add-flows", "br-ex", "-"}
	if got := rec.findOfctl(); !reflect.DeepEqual(got[len(got)-1], wantPlain) {
		t.Errorf("expected the batch to fall back to a plain add-flows, got %v", got)
	}

	// Second cycle: the probe succeeds again and the bundle is back.
	delete(rec.responses, stdinKey([]string{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"}, ""))
	targets["5.182.234.199"] = HairpinTarget{RouterMAC: "fa:16:3e:6f:a1:65"}
	if err := rm.ReconcileOVSHairpinFlows(targets); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if rm.ofctlBundleOK == nil || !*rm.ofctlBundleOK {
		t.Errorf("expected the re-probe to restore the bundle mode, got %v", rm.ofctlBundleOK)
	}
	if n := strings.Count(logs.String(), "--bundle unavailable"); n != 1 {
		t.Errorf("expected exactly 1 bundle warning, got %d; log:\n%s", n, logs.String())
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

func TestRunOVSStdinWithoutHookPipesInput(t *testing.T) {
	// Sanity check that runOVSStdin without a hook really hands its input to
	// the process — the wrapper stdin probe and the batched add are only
	// worth anything if it does.
	rm := &RouteManager{}
	out, err := rm.runOVSStdin([]byte("probe\n"), "cat")
	if err != nil {
		// Some hermetic CI environments lack /bin/cat; treat as skip.
		t.Skipf("`cat` binary unavailable in test env: %v (output: %s)", err, out)
	}
	if strings.TrimSpace(string(out)) != "probe" {
		t.Errorf("runOVSStdin() = %q, want the piped input echoed back", string(out))
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

	// Commands carrying stdin are recorded with it, and a response may be
	// keyed on the pair — which is how a bundle probe and a real bundled add
	// are told apart.
	rec.onStdin([]string{"ovs-ofctl", "--bundle", "add-flows", "br-ex", "-"}, "flow\n", "applied", nil)
	batch := exec.Command("ovs-ofctl", "--bundle", "add-flows", "br-ex", "-")
	batch.Stdin = strings.NewReader("flow\n")
	out, err = hook(batch)
	if err != nil || string(out) != "applied" {
		t.Fatalf("stdin-keyed response = (%q, %v), want (\"applied\", nil)", string(out), err)
	}
	if rec.stdins[1] != "flow\n" {
		t.Errorf("recorded stdin = %q, want %q", rec.stdins[1], "flow\n")
	}
	if got := rec.findBatchedFlows(); !reflect.DeepEqual(got, []string{"flow"}) {
		t.Errorf("findBatchedFlows() = %v, want [flow]", got)
	}
}

// Compile-time check that the recorder hook has the right signature.
var _ ovsExecFunc = (&ovsRecorder{}).hook()
