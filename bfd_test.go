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
