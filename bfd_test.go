package main

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// A tunnel with the OVS default timers (ovn-controller sets only
// bfd:enable=true) and one with the timers this agent can manage.
const (
	geneveFindDefaultTimers = `{"data":[["genev_sys_6081",["map",[["enable","true"]]]]],"headings":["name","bfd"]}`
	geneveFindTunedTimers   = `{"data":[["genev_sys_6081",["map",[["enable","true"],["min_rx","150"],["min_tx","150"]]]]],"headings":["name","bfd"]}`
)

func TestParseGeneveTunnels(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    []bfdTunnel
		wantErr bool
	}{
		{
			name: "explicit timers",
			out:  geneveFindTunedTimers,
			want: []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 150, MinTxMs: 150, Mult: 3}},
		},
		{
			name: "absent keys fall back to OVS defaults",
			out:  geneveFindDefaultTimers,
			want: []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3}},
		},
		{
			name: "explicit mult overrides the default",
			out:  `{"data":[["genev_sys_6081",["map",[["enable","true"],["mult","5"]]]]],"headings":["name","bfd"]}`,
			want: []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 5}},
		},
		{
			name: "enable absent means no BFD session",
			out:  `{"data":[["genev_sys_6081",["map",[]]]],"headings":["name","bfd"]}`,
			want: []bfdTunnel{{Name: "genev_sys_6081", Enabled: false, MinRxMs: 1000, MinTxMs: 100, Mult: 3}},
		},
		{
			name: "non-integer timer falls back to the OVS default",
			out:  `{"data":[["genev_sys_6081",["map",[["enable","true"],["min_rx","fast"],["min_tx","0"]]]]],"headings":["name","bfd"]}`,
			want: []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3}},
		},
		{
			name: "several tunnels",
			out: `{"data":[["genev_sys_6081",["map",[["enable","true"]]]],` +
				`["genev_sys_6082",["map",[["enable","true"],["min_rx","150"],["min_tx","150"]]]]],"headings":["name","bfd"]}`,
			want: []bfdTunnel{
				{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3},
				{Name: "genev_sys_6082", Enabled: true, MinRxMs: 150, MinTxMs: 150, Mult: 3},
			},
		},
		{
			name: "no geneve tunnels",
			out:  `{"data":[],"headings":["name","bfd"]}`,
			want: nil,
		},
		{"empty output", "", nil, false},
		{"blank output", "   \n", nil, false},
		{"malformed json", `{"data":`, nil, true},
		{"missing bfd column", `{"data":[["genev_sys_6081"]],"headings":["name"]}`, nil, true},
		{"row shorter than headings", `{"data":[["genev_sys_6081"]],"headings":["name","bfd"]}`, nil, true},
		{"bfd column is not a map", `{"data":[["genev_sys_6081",["set",[]]]],"headings":["name","bfd"]}`, nil, true},
		{"bfd column is a bare scalar", `{"data":[["genev_sys_6081","nope"]],"headings":["name","bfd"]}`, nil, true},
		{"interface name is not a string", `{"data":[[42,["map",[]]]],"headings":["name","bfd"]}`, nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGeneveTunnels([]byte(tt.out))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseGeneveTunnels() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d tunnels, want %d (%+v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("tunnel[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBFDTunnelDetectTime(t *testing.T) {
	tests := []struct {
		name   string
		tunnel bfdTunnel
		want   time.Duration
	}{
		{"OVS defaults are the documented 3s floor", bfdTunnel{MinRxMs: 1000, MinTxMs: 100, Mult: 3}, 3 * time.Second},
		{"tuned 150/150 with mult 3", bfdTunnel{MinRxMs: 150, MinTxMs: 150, Mult: 3}, 450 * time.Millisecond},
		{"the slower interval wins", bfdTunnel{MinRxMs: 150, MinTxMs: 900, Mult: 3}, 2700 * time.Millisecond},
		{"higher multiplier extends detection", bfdTunnel{MinRxMs: 150, MinTxMs: 150, Mult: 5}, 750 * time.Millisecond},
		{"an unreported multiplier yields no estimate", bfdTunnel{MinRxMs: 150, MinTxMs: 150}, 0},
		// The bfd column accepts any integer literal, so a fat-fingered timer
		// reaches this code. The nanosecond product must saturate rather than
		// wrap to a negative duration.
		{
			"an out-of-range interval saturates",
			bfdTunnel{MinRxMs: 1_000_000_000, MinTxMs: 1, Mult: 1_000_000},
			maxBFDDetectTime,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tunnel.detectTime(); got != tt.want {
				t.Errorf("detectTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Only tunnels running a BFD session bound the failover floor; a disabled
// tunnel with terrible timers must not be reported.
func TestWorstTunnelDetectTime(t *testing.T) {
	tests := []struct {
		name     string
		tunnels  []bfdTunnel
		want     time.Duration
		wantName string
	}{
		{"no tunnels", nil, 0, ""},
		{
			"disabled tunnels are ignored",
			[]bfdTunnel{{Name: "a", Enabled: false, MinRxMs: 5000, MinTxMs: 5000, Mult: 3}},
			0, "",
		},
		{
			"worst enabled tunnel wins",
			[]bfdTunnel{
				{Name: "fast", Enabled: true, MinRxMs: 150, MinTxMs: 150, Mult: 3},
				{Name: "slow", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3},
			},
			3 * time.Second, "slow",
		},
		{
			"a disabled slow tunnel does not mask an enabled fast one",
			[]bfdTunnel{
				{Name: "slow", Enabled: false, MinRxMs: 5000, MinTxMs: 5000, Mult: 3},
				{Name: "fast", Enabled: true, MinRxMs: 150, MinTxMs: 150, Mult: 3},
			},
			450 * time.Millisecond, "fast",
		},
		{
			// A tunnel whose timers overflow the nanosecond product has an
			// effectively infinite detection time — it is exactly the one the
			// check must report, not the one it silently drops.
			"a tunnel with out-of-range timers is not discarded",
			[]bfdTunnel{
				{Name: "fast", Enabled: true, MinRxMs: 150, MinTxMs: 150, Mult: 3},
				{Name: "broken", Enabled: true, MinRxMs: 1_000_000_000, MinTxMs: 1, Mult: 1_000_000},
			},
			maxBFDDetectTime, "broken",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, name := worstTunnelDetectTime(tt.tunnels)
			if got != tt.want || name != tt.wantName {
				t.Errorf("worstTunnelDetectTime() = (%v, %q), want (%v, %q)", got, name, tt.want, tt.wantName)
			}
		})
	}
}

func TestGeneveTunnels(t *testing.T) {
	const findCmd = "ovs-vsctl --format=json --columns=name,bfd find Interface type=geneve"

	t.Run("enumerates the local tunnels", func(t *testing.T) {
		rec := newOVSRecorder()
		rec.on(strings.Fields(findCmd), geneveFindTunedTimers, nil)
		rm := &RouteManager{execOVSHook: rec.hook()}

		tunnels, err := rm.GeneveTunnels()
		if err != nil {
			t.Fatalf("GeneveTunnels() error: %v", err)
		}
		if len(tunnels) != 1 || tunnels[0].Name != "genev_sys_6081" {
			t.Fatalf("got %+v, want one genev_sys_6081 tunnel", tunnels)
		}
		if len(rec.calls) != 1 || strings.Join(rec.calls[0], " ") != findCmd {
			t.Fatalf("calls = %v, want exactly %q", rec.calls, findCmd)
		}
	})

	t.Run("a node with no tunnels yields none", func(t *testing.T) {
		rec := newOVSRecorder()
		rec.on(strings.Fields(findCmd), `{"data":[],"headings":["name","bfd"]}`, nil)
		rm := &RouteManager{execOVSHook: rec.hook()}

		tunnels, err := rm.GeneveTunnels()
		if err != nil {
			t.Fatalf("GeneveTunnels() error: %v", err)
		}
		if len(tunnels) != 0 {
			t.Errorf("tunnels = %+v, want none", tunnels)
		}
	})

	t.Run("exec error is propagated", func(t *testing.T) {
		rec := newOVSRecorder()
		rec.on(strings.Fields(findCmd), "ovs-vsctl: unix socket error", errors.New("exit status 1"))
		rm := &RouteManager{execOVSHook: rec.hook()}

		if _, err := rm.GeneveTunnels(); err == nil {
			t.Fatal("GeneveTunnels() = nil error, want the exec failure")
		}
	})

	t.Run("malformed output is an error", func(t *testing.T) {
		rec := newOVSRecorder()
		rec.on(strings.Fields(findCmd), `{"data":`, nil)
		rm := &RouteManager{execOVSHook: rec.hook()}

		if _, err := rm.GeneveTunnels(); err == nil {
			t.Fatal("GeneveTunnels() = nil error, want a parse failure")
		}
	})
}

func TestFRRBFDPeerDetectTime(t *testing.T) {
	tests := []struct {
		name string
		peer frrBFDPeer
		want time.Duration
	}{
		{
			"remote transmit interval refines the estimate",
			frrBFDPeer{ReceiveInterval: 300, TransmitInterval: 300, RemoteTransmitInterval: 1000, DetectMultiplier: 3},
			3 * time.Second,
		},
		{
			"falls back to our transmit interval before the session is up",
			frrBFDPeer{ReceiveInterval: 150, TransmitInterval: 150, DetectMultiplier: 3},
			450 * time.Millisecond,
		},
		{
			"our receive interval wins when it is the slower side",
			frrBFDPeer{ReceiveInterval: 900, TransmitInterval: 150, RemoteTransmitInterval: 150, DetectMultiplier: 3},
			2700 * time.Millisecond,
		},
		{
			"an unreported multiplier yields no estimate",
			frrBFDPeer{ReceiveInterval: 300, TransmitInterval: 300},
			0,
		},
		{
			"an out-of-range interval saturates",
			frrBFDPeer{ReceiveInterval: 1_000_000_000, TransmitInterval: 1, DetectMultiplier: 1_000_000},
			maxBFDDetectTime,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.peer.detectTime(); got != tt.want {
				t.Errorf("detectTime() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListFRRBFDPeers(t *testing.T) {
	const peersCmd = "vtysh -c show bfd peers json"
	peersJSON := `[{"peer":"192.0.2.1","vrf":"vrf-provider","status":"up",` +
		`"receive-interval":300,"transmit-interval":300,"remote-transmit-interval":300,"detect-multiplier":3}]`

	tests := []struct {
		name      string
		out       string
		execErr   error
		wantPeers int
		wantErr   bool
	}{
		{"peers parsed", peersJSON, nil, 1, false},
		{"empty json array", `[]`, nil, 0, false},
		// A fabric without BFD, or a node where bfdd is not running, is an
		// expected state for a default-on check — not an error.
		{"bfdd not running", "% BFD is not running\n", nil, 0, false},
		{"unknown command", "% Unknown command: show bfd peers json", nil, 0, false},
		{"empty output", "", nil, 0, false},
		{"exec failure", "vtysh: connect failed", errors.New("exit status 1"), 0, true},
		{"malformed json array", `[{"peer":`, nil, 0, true},
		// Reading unparseable output as "no peers" would report a fabric whose
		// BFD is healthy as running none, flipping the gauge to +Inf.
		{"contaminated json is an error, not an empty peer list", "vtysh: warning\n" + peersJSON, nil, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newVtyshRecorder()
			rec.on([]string{"vtysh", "-c", "show bfd peers json"}, tt.out, tt.execErr)
			rm := &RouteManager{vrfName: "vrf-provider", execVtyshHook: rec.hook()}

			peers, err := rm.ListFRRBFDPeers()
			if (err != nil) != tt.wantErr {
				t.Fatalf("ListFRRBFDPeers() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(peers) != tt.wantPeers {
				t.Errorf("got %d peers, want %d", len(peers), tt.wantPeers)
			}
			if len(rec.calls) != 1 || strings.Join(rec.calls[0], " ") != peersCmd {
				t.Errorf("calls = %v, want exactly %q", rec.calls, peersCmd)
			}
		})
	}
}

// Peers outside the agent's VRF belong to another routing domain and must not
// drive this agent's estimate.
func TestWorstPeerDetectTime(t *testing.T) {
	peers := []frrBFDPeer{
		{Peer: "192.0.2.1", VRF: "vrf-provider", ReceiveInterval: 150, TransmitInterval: 150, DetectMultiplier: 3},
		{Peer: "198.51.100.1", VRF: "default", ReceiveInterval: 1000, TransmitInterval: 1000, DetectMultiplier: 3},
	}

	got, name := worstPeerDetectTime(peers, "vrf-provider")
	if got != 450*time.Millisecond || name != "192.0.2.1" {
		t.Errorf("worstPeerDetectTime() = (%v, %q), want (450ms, %q)", got, name, "192.0.2.1")
	}

	if got, name := worstPeerDetectTime(peers, "vrf-absent"); got != 0 || name != "" {
		t.Errorf("worstPeerDetectTime(absent vrf) = (%v, %q), want (0, \"\")", got, name)
	}
	if got, _ := worstPeerDetectTime(nil, "vrf-provider"); got != 0 {
		t.Errorf("worstPeerDetectTime(nil) = %v, want 0", got)
	}
}

// bfdd omits fields it has not negotiated, so an absent vrf means "unknown".
// Comparing it against the configured VRF discards a healthy session and makes
// the check report that nothing bounds the failover.
func TestWorstPeerDetectTimeKeepsPeersWithoutAReportedVRF(t *testing.T) {
	peers := []frrBFDPeer{
		{Peer: "192.0.2.1", ReceiveInterval: 150, TransmitInterval: 150, DetectMultiplier: 3},
	}

	got, name := worstPeerDetectTime(peers, "vrf-provider")
	if got != 450*time.Millisecond || name != "192.0.2.1" {
		t.Errorf("worstPeerDetectTime() = (%v, %q), want (450ms, %q) — an unreported vrf is unknown, not foreign",
			got, name, "192.0.2.1")
	}
}

// bfdTestAgent builds an agent whose OVS and vtysh calls are recorded, with
// one default-timer Geneve tunnel and one slow FRR BFD peer.
func bfdTestAgent(t *testing.T, cfg Config) (*Agent, *ovsRecorder, *vtyshRecorder) {
	t.Helper()
	ovsRec := newOVSRecorder()
	ovsRec.on(strings.Fields("ovs-vsctl --format=json --columns=name,bfd find Interface type=geneve"),
		geneveFindDefaultTimers, nil)

	vtyshRec := newVtyshRecorder()
	vtyshRec.on([]string{"vtysh", "-c", "show bfd peers json"},
		`[{"peer":"192.0.2.1","vrf":"vrf-provider","status":"up",`+
			`"receive-interval":300,"transmit-interval":300,"remote-transmit-interval":1000,"detect-multiplier":3}]`, nil)

	cfg.VRFName = "vrf-provider"
	rm := &RouteManager{vrfName: "vrf-provider", execOVSHook: ovsRec.hook(), execVtyshHook: vtyshRec.hook()}
	return &Agent{cfg: cfg, routing: rm}, ovsRec, vtyshRec
}

// mutatingCommands returns the recorded calls that would change OVS or FRR
// state — the check must never issue any of them.
func mutatingCommands(calls [][]string) []string {
	var found []string
	for _, c := range calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "set Interface") ||
			strings.Contains(joined, "conf t") ||
			strings.Contains(joined, "router bgp") {
			found = append(found, joined)
		}
	}
	return found
}

// With defaults the agent estimates and exports the detection time but leaves
// OVS and FRR untouched.
func TestReconcileBFDDefaultsIsReadOnly(t *testing.T) {
	m := withTestMetrics(t)
	a, ovsRec, vtyshRec := bfdTestAgent(t, Config{BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second})

	a.reconcileBFD()

	if got := mutatingCommands(ovsRec.calls); len(got) > 0 {
		t.Errorf("check modified OVS state: %v", got)
	}
	if got := mutatingCommands(vtyshRec.calls); len(got) > 0 {
		t.Errorf("check modified FRR state: %v", got)
	}

	// Default OVS timers → 3 × 1000 ms; the FRR peer's remote transmit
	// interval of 1000 ms → 3 × 1000 ms as well.
	if v, ok := bfdDetectValue(t, m, "ovn"); !ok || v != 3 {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v (present=%v), want 3", v, ok)
	}
	if v, ok := bfdDetectValue(t, m, "frr"); !ok || v != 3 {
		t.Errorf("bfd_detect_seconds{layer=frr} = %v (present=%v), want 3", v, ok)
	}
}

func TestReconcileBFDCheckDisabledIssuesNoCommands(t *testing.T) {
	withTestMetrics(t)
	a, ovsRec, vtyshRec := bfdTestAgent(t, Config{BFDCheckEnabled: false})

	a.reconcileBFD()

	if len(ovsRec.calls) != 0 {
		t.Errorf("OVS calls = %v, want none when the check is disabled", ovsRec.calls)
	}
	if len(vtyshRec.calls) != 0 {
		t.Errorf("vtysh calls = %v, want none when the check is disabled", vtyshRec.calls)
	}
}

// Port-forward-only nodes run no OVS, so the OVN layer is skipped — but FRR
// still announces the VIP /32s there, so its layer must still run.
func TestReconcileBFDPortForwardOnlySkipsOVNLayer(t *testing.T) {
	m := withTestMetrics(t)
	a, ovsRec, vtyshRec := bfdTestAgent(t, Config{
		BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second, PortForwardOnly: true,
	})

	a.reconcileBFD()

	if len(ovsRec.calls) != 0 {
		t.Errorf("OVS calls = %v, want none in port-forward-only mode", ovsRec.calls)
	}
	if len(vtyshRec.calls) == 0 {
		t.Error("the FRR layer must still run in port-forward-only mode")
	}
	if v, _ := bfdDetectValue(t, m, "ovn"); !math.IsNaN(v) {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v, want NaN (never measured)", v)
	}
	if v, _ := bfdDetectValue(t, m, "frr"); v != 3 {
		t.Errorf("bfd_detect_seconds{layer=frr} = %v, want 3", v)
	}
}

// A failing layer is logged and skipped; it must not abort the other layer or
// leave a stale estimate behind. Its gauge goes to NaN, not to 0: an
// ovsdb-server the agent cannot reach says nothing about the detection time,
// and 0 would read as the fastest possible one.
func TestReconcileBFDToleratesLayerFailures(t *testing.T) {
	m := withTestMetrics(t)
	ovsRec := newOVSRecorder()
	ovsRec.on(strings.Fields("ovs-vsctl --format=json --columns=name,bfd find Interface type=geneve"),
		"ovs-vsctl: database connection failed", errors.New("exit status 1"))
	vtyshRec := newVtyshRecorder()
	vtyshRec.on([]string{"vtysh", "-c", "show bfd peers json"},
		`[{"peer":"192.0.2.1","vrf":"vrf-provider","receive-interval":150,"transmit-interval":150,"detect-multiplier":3}]`, nil)

	rm := &RouteManager{vrfName: "vrf-provider", execOVSHook: ovsRec.hook(), execVtyshHook: vtyshRec.hook()}
	a := &Agent{cfg: Config{BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second, VRFName: "vrf-provider"}, routing: rm}

	a.reconcileBFD()

	if v, _ := bfdDetectValue(t, m, "ovn"); !math.IsNaN(v) {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v, want NaN after an enumeration failure", v)
	}
	if got := bfdCheckErrorCount(t, m, "ovn"); got != 1 {
		t.Errorf("bfd_check_errors_total{layer=ovn} = %v, want 1", got)
	}
	if v, _ := bfdDetectValue(t, m, "frr"); v != 0.45 {
		t.Errorf("bfd_detect_seconds{layer=frr} = %v, want 0.45 — the FRR layer must still run", v)
	}
	if got := bfdCheckErrorCount(t, m, "frr"); got != 0 {
		t.Errorf("bfd_check_errors_total{layer=frr} = %v, want 0 — that layer was read fine", got)
	}
}

// A node whose tunnels run no BFD session, and a VRF with no BFD peer, are the
// worst states this check can find: nothing bounds the failover at all. They
// must not share the gauge value of the best state.
func TestReconcileBFDNoSessionReportsUnboundedDetection(t *testing.T) {
	m := withTestMetrics(t)
	ovsRec := newOVSRecorder()
	ovsRec.on(strings.Fields("ovs-vsctl --format=json --columns=name,bfd find Interface type=geneve"),
		`{"data":[["genev_sys_6081",["map",[]]]],"headings":["name","bfd"]}`, nil)
	vtyshRec := newVtyshRecorder()
	vtyshRec.on([]string{"vtysh", "-c", "show bfd peers json"}, "% BFD is not running\n", nil)

	rm := &RouteManager{vrfName: "vrf-provider", execOVSHook: ovsRec.hook(), execVtyshHook: vtyshRec.hook()}
	a := &Agent{cfg: Config{BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second, VRFName: "vrf-provider"}, routing: rm}

	a.reconcileBFD()

	for _, layer := range []string{"ovn", "frr"} {
		v, _ := bfdDetectValue(t, m, layer)
		if !math.IsInf(v, 1) {
			t.Errorf("bfd_detect_seconds{layer=%s} = %v, want +Inf — no BFD session bounds the failover", layer, v)
		}
		// Nothing failed to read, so this is not an error state.
		if got := bfdCheckErrorCount(t, m, layer); got != 0 {
			t.Errorf("bfd_check_errors_total{layer=%s} = %v, want 0", layer, got)
		}
	}
}

// A gauge that keeps its last good value once measurement stops lets an alert
// on bfd_detect_seconds stay quiet, and a dashboard read healthy, while the
// agent has measured nothing for hours.
func TestReconcileBFDClearsTheEstimateWhenMeasurementStops(t *testing.T) {
	m := withTestMetrics(t)
	a, ovsRec, vtyshRec := bfdTestAgent(t, Config{BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second})

	a.reconcileBFD()
	for _, layer := range []string{"ovn", "frr"} {
		if v, ok := bfdDetectValue(t, m, layer); !ok || v != 3 {
			t.Fatalf("bfd_detect_seconds{layer=%s} = %v (present=%v), want 3 before the failure", layer, v, ok)
		}
	}

	// ovsdb-server became unreachable — the OVN layer is now unknown. bfdd is
	// still answering, and it reports no session at all — the FRR layer is
	// known, and unbounded.
	ovsRec.on(strings.Fields("ovs-vsctl --format=json --columns=name,bfd find Interface type=geneve"),
		"ovs-vsctl: database connection failed", errors.New("exit status 1"))
	vtyshRec.on([]string{"vtysh", "-c", "show bfd peers json"}, `[]`, nil)

	a.reconcileBFD()

	if v, _ := bfdDetectValue(t, m, "ovn"); !math.IsNaN(v) {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v, want NaN — a stale estimate must not survive an unreadable layer", v)
	}
	if v, _ := bfdDetectValue(t, m, "frr"); !math.IsInf(v, 1) {
		t.Errorf("bfd_detect_seconds{layer=frr} = %v, want +Inf — a stale estimate must not survive a vanished session", v)
	}
}

// =============================================================================
// OVN tunnel BFD timer management (ovn_bfd_manage)
// =============================================================================

// setTimerCommands returns the recorded `ovs-vsctl set Interface … bfd:…` calls.
func setTimerCommands(calls [][]string) []string {
	var found []string
	for _, c := range calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "set Interface") && strings.Contains(joined, "bfd:min_rx") {
			found = append(found, joined)
		}
	}
	return found
}

func TestEnsureOVNBFDTimers(t *testing.T) {
	t.Run("a drifted tunnel is written, and the caller's slice is left alone", func(t *testing.T) {
		rec := newOVSRecorder()
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3}}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		want := "ovs-vsctl --if-exists set Interface genev_sys_6081 bfd:min_rx=150 bfd:min_tx=150"
		if got := setTimerCommands(rec.calls); len(got) != 1 || got[0] != want {
			t.Fatalf("commands = %v, want exactly [%q]", got, want)
		}

		// A successful write is not proof that OVS kept the value: ovn-controller
		// may reclaim the bfd column. Only a fresh read knows the effective
		// timers, so the slice must still hold what OVSDB reported.
		if tunnels[0].MinRxMs != 1000 || tunnels[0].MinTxMs != 100 {
			t.Errorf("tunnel = %+v, want the timers OVS reported, not the ones written", tunnels[0])
		}
	})

	t.Run("a tunnel already at the desired timers is not written", func(t *testing.T) {
		rec := newOVSRecorder()
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 150, MinTxMs: 150, Mult: 3}}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		if got := setTimerCommands(rec.calls); len(got) != 0 {
			t.Errorf("commands = %v, want none", got)
		}
	})

	// ovsCommandTimeout bounds one ovs-vsctl invocation, not one reconcile. A
	// per-tunnel loop over a cluster's worth of drifted tunnels therefore has no
	// bound at all: a wedged ovsdb-server would hold the agent's only loop
	// goroutine for 30 s per remote chassis, blocking route programming and the
	// SIGTERM drain for hours. Every drifted tunnel goes into one transaction,
	// and --if-exists keeps a tunnel ovn-controller destroyed since the
	// enumeration from failing it.
	t.Run("every drifted tunnel is written by a single ovs-vsctl invocation", func(t *testing.T) {
		rec := newOVSRecorder()
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{
			{Name: "gone", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3},
			{Name: "survivor", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3},
		}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		if len(rec.calls) != 1 {
			t.Fatalf("issued %d ovs-vsctl calls, want exactly one: %v", len(rec.calls), rec.calls)
		}
		want := "ovs-vsctl --if-exists set Interface gone bfd:min_rx=150 bfd:min_tx=150 " +
			"-- --if-exists set Interface survivor bfd:min_rx=150 bfd:min_tx=150"
		if got := strings.Join(rec.calls[0], " "); got != want {
			t.Errorf("command = %q, want %q", got, want)
		}
	})

	t.Run("only the drifted tunnel is written", func(t *testing.T) {
		rec := newOVSRecorder()
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{
			{Name: "correct", Enabled: true, MinRxMs: 150, MinTxMs: 150, Mult: 3},
			{Name: "drifted", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3},
		}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		got := setTimerCommands(rec.calls)
		if len(got) != 1 || !strings.Contains(got[0], "Interface drifted") {
			t.Errorf("commands = %v, want only the drifted tunnel written", got)
		}
	})

	t.Run("a disabled tunnel is still pre-staged", func(t *testing.T) {
		// ovn-controller flips bfd:enable when a gateway lands here; having
		// the timers already set means the session starts out fast.
		rec := newOVSRecorder()
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{{Name: "idle", Enabled: false, MinRxMs: 1000, MinTxMs: 100, Mult: 3}}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		if got := setTimerCommands(rec.calls); len(got) != 1 {
			t.Errorf("commands = %v, want the idle tunnel pre-staged", got)
		}
	})

	// `--` is ovs-vsctl's own clause separator: an Interface row named `--`, or
	// anything starting with `-`, re-partitions the command line and fails the
	// transaction, leaving every tunnel in the batch untuned.
	t.Run("an interface whose name would re-partition the command line is skipped", func(t *testing.T) {
		rec := newOVSRecorder()
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{
			{Name: "--", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3},
			{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3},
		}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		want := "ovs-vsctl --if-exists set Interface genev_sys_6081 bfd:min_rx=150 bfd:min_tx=150"
		if got := setTimerCommands(rec.calls); len(got) != 1 || got[0] != want {
			t.Fatalf("commands = %v, want exactly [%q] — the poisoned row must not disable the whole batch", got, want)
		}
	})

	t.Run("nothing is issued when every name is unusable", func(t *testing.T) {
		rec := newOVSRecorder()
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{{Name: "-x", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3}}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		if len(rec.calls) != 0 {
			t.Errorf("issued %v, want no ovs-vsctl invocation at all", rec.calls)
		}
	})

	t.Run("exec error is propagated", func(t *testing.T) {
		rec := newOVSRecorder()
		rec.on(strings.Fields("ovs-vsctl --if-exists set Interface genev_sys_6081 bfd:min_rx=150 bfd:min_tx=150"),
			"ovs-vsctl: unix socket error", errors.New("exit status 1"))
		rm := &RouteManager{execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3}}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err == nil {
			t.Fatal("EnsureOVNBFDTimers() = nil error, want the exec failure")
		}
	})

	t.Run("dry-run writes nothing", func(t *testing.T) {
		rec := newOVSRecorder()
		rm := &RouteManager{dryRun: true, execOVSHook: rec.hook()}
		tunnels := []bfdTunnel{{Name: "genev_sys_6081", Enabled: true, MinRxMs: 1000, MinTxMs: 100, Mult: 3}}

		if err := rm.EnsureOVNBFDTimers(tunnels, 150, 150); err != nil {
			t.Fatalf("EnsureOVNBFDTimers() error: %v", err)
		}
		if len(rec.calls) != 0 {
			t.Errorf("dry-run issued %v, want no commands", rec.calls)
		}
	})
}

// The exported estimate must describe the timers OVS holds, not the ones the
// agent asked for — otherwise the check that exists to prove ovn_bfd_manage
// took effect is structurally incapable of observing that it did not.
func TestReconcileBFDOVNManageEstimatesFromOVSDBNotFromIntent(t *testing.T) {
	m := withTestMetrics(t)
	a, ovsRec, vtyshRec := bfdTestAgent(t, Config{
		BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second,
		OVNBFDManage: true, OVNBFDMinRxMs: 150, OVNBFDMinTxMs: 150,
	})

	a.reconcileBFD()

	if got := setTimerCommands(ovsRec.calls); len(got) != 1 {
		t.Fatalf("OVS timer commands = %v, want exactly one", got)
	}
	// OVS still reported the untuned timers this cycle, so 3 s is the floor
	// that was actually in force.
	if v, _ := bfdDetectValue(t, m, "ovn"); v != 3 {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v, want 3 — the estimate must follow OVSDB, not the write", v)
	}
	// FRR must stay untouched: the manage flags are independent.
	if got := mutatingCommands(vtyshRec.calls); len(got) > 0 {
		t.Errorf("ovn_bfd_manage modified FRR state: %v", got)
	}

	// Once OVS reports the timers that were written, the next reconcile finds
	// nothing drifted, writes nothing, and the estimate follows.
	ovsRec.on(strings.Fields("ovs-vsctl --format=json --columns=name,bfd find Interface type=geneve"),
		geneveFindTunedTimers, nil)
	before := len(setTimerCommands(ovsRec.calls))
	a.reconcileBFD()
	if got := setTimerCommands(ovsRec.calls); len(got) != before {
		t.Errorf("second reconcile issued %d timer commands, want 0", len(got)-before)
	}
	if v, _ := bfdDetectValue(t, m, "ovn"); v != 0.45 {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v, want 0.45 once OVS reports the tuned timers", v)
	}
}

// An OVN version that reclaims the bfd column reverts the timers after every
// write. The metric must keep reporting the real 3 s floor rather than the
// 450 ms the agent keeps asking for.
func TestReconcileBFDOVNManageReportsRevertedTimers(t *testing.T) {
	m := withTestMetrics(t)
	a, ovsRec, _ := bfdTestAgent(t, Config{
		BFDCheckEnabled: true, BFDCheckMaxDetect: time.Second,
		OVNBFDManage: true, OVNBFDMinRxMs: 150, OVNBFDMinTxMs: 150,
	})

	// The recorder keeps answering the find with the OVS defaults: whatever the
	// agent writes, ovn-controller puts them back.
	a.reconcileBFD()
	a.reconcileBFD()

	if got := setTimerCommands(ovsRec.calls); len(got) != 2 {
		t.Errorf("OVS timer commands = %v, want one per reconcile", got)
	}
	if v, _ := bfdDetectValue(t, m, "ovn"); v != 3 {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v, want 3 — a reverted write must stay visible", v)
	}
}

// The manager runs even when the check is off — the two are independent.
func TestReconcileBFDOVNManageWithoutCheck(t *testing.T) {
	m := withTestMetrics(t)
	a, ovsRec, _ := bfdTestAgent(t, Config{
		BFDCheckEnabled: false,
		OVNBFDManage:    true, OVNBFDMinRxMs: 150, OVNBFDMinTxMs: 150,
	})

	a.reconcileBFD()

	if got := setTimerCommands(ovsRec.calls); len(got) != 1 {
		t.Errorf("OVS timer commands = %v, want exactly one", got)
	}
	if v, _ := bfdDetectValue(t, m, "ovn"); !math.IsNaN(v) {
		t.Errorf("bfd_detect_seconds{layer=ovn} = %v, want NaN — the check is disabled", v)
	}
}
