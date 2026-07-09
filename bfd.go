package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"
)

// Ungraceful failover (node crash, partition, ovn-controller crash) is
// dominated by BFD's detection of the dead gateway chassis: OVN cannot move a
// chassisredirect Port_Binding before BFD declares the tunnel down, and the
// fabric keeps sending traffic to a crashed node until its BGP session drops.
// This file estimates that detection floor per layer, and optionally lowers it.

// OVS BFD timer defaults from ovs-vswitchd.conf.db(5). ovn-controller sets
// only bfd:enable=true, so an untuned tunnel negotiates 3 × 1000 ms — the
// ~3 s floor documented in docs/explanation/gateway-drain.md.
const (
	ovsBFDDefaultMinRxMs = 1000
	ovsBFDDefaultMinTxMs = 100
	ovsBFDDefaultMult    = 3
)

// maxBFDDetectTime saturates an estimate that would otherwise overflow.
// time.Duration is int64 nanoseconds, while the timers arrive as free-form
// integers — OVSDB's bfd column is a map<string,string> and bfdd reports plain
// JSON numbers. An out-of-range interval must not wrap to a negative duration,
// because every worst-case comparison below would then silently discard the
// session with the longest detection time.
const (
	maxBFDDetectTime       = time.Duration(1<<63 - 1)
	maxBFDDetectIntervalMs = int64(maxBFDDetectTime) / int64(time.Millisecond)
)

// bfdDetectTime is how long a BFD session takes to declare its peer dead: the
// detect multiplier times the negotiated interval. An unreported multiplier or
// interval (0) yields no estimate.
func bfdDetectTime(mult, intervalMs int) time.Duration {
	if mult <= 0 || intervalMs <= 0 {
		return 0
	}
	if int64(intervalMs) > maxBFDDetectIntervalMs/int64(mult) {
		return maxBFDDetectTime
	}
	return time.Duration(mult) * time.Duration(intervalMs) * time.Millisecond
}

// =============================================================================
// OVN Geneve tunnel BFD
// =============================================================================

// bfdTunnel is one local Geneve tunnel interface with its BFD configuration,
// as read from the Interface table's bfd column.
type bfdTunnel struct {
	Name    string
	Enabled bool // bfd:enable=true — ovn-controller runs a BFD session here
	MinRxMs int
	MinTxMs int
	Mult    int
}

// detectTime estimates how long this tunnel takes to declare the remote
// endpoint dead: the detect multiplier times the slower of the two intervals.
// Only the local side's configuration is visible, so this is a lower bound —
// the negotiated interval is the maximum across both endpoints, which is why
// timers must be lowered fleet-wide to take effect.
func (t bfdTunnel) detectTime() time.Duration {
	interval := t.MinRxMs
	if t.MinTxMs > interval {
		interval = t.MinTxMs
	}
	return bfdDetectTime(t.Mult, interval)
}

// ovsFindResult is the `ovs-vsctl --format=json find` envelope: one row per
// record, with the columns positioned by Headings.
type ovsFindResult struct {
	Data     [][]json.RawMessage `json:"data"`
	Headings []string            `json:"headings"`
}

// ovsdbMap decodes the OVSDB JSON map notation `["map",[["k","v"],…]]`.
func ovsdbMap(raw json.RawMessage) (map[string]string, error) {
	var wrapper []json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("decode map wrapper: %w", err)
	}
	if len(wrapper) != 2 {
		return nil, fmt.Errorf("expected a 2-element OVSDB map, got %d elements", len(wrapper))
	}
	var kind string
	if err := json.Unmarshal(wrapper[0], &kind); err != nil {
		return nil, fmt.Errorf("decode map kind: %w", err)
	}
	if kind != "map" {
		return nil, fmt.Errorf("expected an OVSDB map, got %q", kind)
	}
	var pairs [][2]string
	if err := json.Unmarshal(wrapper[1], &pairs); err != nil {
		return nil, fmt.Errorf("decode map pairs: %w", err)
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		m[p[0]] = p[1]
	}
	return m, nil
}

// bfdIntOr returns the integer value of key, falling back to the OVS default
// when the key is absent or unusable — which is exactly what OVS itself does.
func bfdIntOr(m map[string]string, key string, fallback int) int {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// parseGeneveTunnels decodes the output of findGeneveInterfaces.
func parseGeneveTunnels(out []byte) ([]bfdTunnel, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var result ovsFindResult
	if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
		return nil, fmt.Errorf("parse ovs-vsctl find json: %w", err)
	}

	nameIdx, bfdIdx := -1, -1
	for i, h := range result.Headings {
		switch h {
		case "name":
			nameIdx = i
		case "bfd":
			bfdIdx = i
		}
	}
	if nameIdx < 0 || bfdIdx < 0 {
		return nil, fmt.Errorf("ovs-vsctl find json is missing the name or bfd column")
	}

	var tunnels []bfdTunnel
	for _, row := range result.Data {
		if nameIdx >= len(row) || bfdIdx >= len(row) {
			return nil, fmt.Errorf("ovs-vsctl find json row has only %d columns", len(row))
		}
		var name string
		if err := json.Unmarshal(row[nameIdx], &name); err != nil {
			return nil, fmt.Errorf("decode interface name: %w", err)
		}
		bfd, err := ovsdbMap(row[bfdIdx])
		if err != nil {
			return nil, fmt.Errorf("decode bfd column of %s: %w", name, err)
		}
		tunnels = append(tunnels, bfdTunnel{
			Name:    name,
			Enabled: bfd["enable"] == "true",
			MinRxMs: bfdIntOr(bfd, "min_rx", ovsBFDDefaultMinRxMs),
			MinTxMs: bfdIntOr(bfd, "min_tx", ovsBFDDefaultMinTxMs),
			Mult:    bfdIntOr(bfd, "mult", ovsBFDDefaultMult),
		})
	}
	return tunnels, nil
}

// GeneveTunnels returns the local Geneve tunnel interfaces.
func (rm *RouteManager) GeneveTunnels() ([]bfdTunnel, error) {
	out, err := rm.findGeneveInterfaces()
	if err != nil {
		return nil, err
	}
	return parseGeneveTunnels(out)
}

// EnsureOVNBFDTimers sets bfd:min_rx and bfd:min_tx on the Geneve tunnel
// interfaces whose timers differ from the desired values. A reconcile that finds
// nothing drifted issues no OVS command at all, and one that finds N drifted
// tunnels issues exactly one — see setInterfaceBFDTimers for why the count
// matters.
//
// tunnels is read, never updated: what the agent writes is not necessarily what
// OVS keeps, so the effective timers are the ones the next reconcile reads back
// from the bfd column.
//
// bfd:enable is never touched: ovn-controller owns it. Whether ovn-controller
// preserves operator-set timer keys depends on the deployed OVN version — if
// it reverts them, the next reconcile re-applies them. See
// docs/explanation/bfd-failover-detection.md.
func (rm *RouteManager) EnsureOVNBFDTimers(tunnels []bfdTunnel, minRxMs, minTxMs int) error {
	if rm.dryRun {
		slog.Info("[dry-run] would set BFD timers on Geneve tunnels",
			"tunnels", len(tunnels), "min_rx_ms", minRxMs, "min_tx_ms", minTxMs)
		return nil
	}
	var drifted []string
	for _, t := range tunnels {
		if t.MinRxMs != minRxMs || t.MinTxMs != minTxMs {
			drifted = append(drifted, t.Name)
		}
	}
	if len(drifted) == 0 {
		return nil
	}
	if err := rm.setInterfaceBFDTimers(drifted, minRxMs, minTxMs); err != nil {
		return err
	}
	slog.Info("BFD timers set on Geneve tunnels",
		"tunnels", len(drifted), "min_rx_ms", minRxMs, "min_tx_ms", minTxMs)
	return nil
}

// worstTunnelDetectTime returns the longest detection time across the tunnels
// that actually run a BFD session, and the tunnel it belongs to. Tunnels
// without bfd:enable=true contribute nothing.
func worstTunnelDetectTime(tunnels []bfdTunnel) (time.Duration, string) {
	var worst time.Duration
	var name string
	for _, t := range tunnels {
		if !t.Enabled {
			continue
		}
		if d := t.detectTime(); d > worst {
			worst, name = d, t.Name
		}
	}
	return worst, name
}

// =============================================================================
// FRR BGP-session BFD
// =============================================================================

// frrBFDPeer is the subset of a `show bfd peers json` entry the agent
// inspects. bfdd omits fields it has not negotiated, so the zero value means
// "unknown" and is never compared.
type frrBFDPeer struct {
	Peer                   string `json:"peer"`
	VRF                    string `json:"vrf"`
	ReceiveInterval        int    `json:"receive-interval"`
	TransmitInterval       int    `json:"transmit-interval"`
	RemoteTransmitInterval int    `json:"remote-transmit-interval"`
	DetectMultiplier       int    `json:"detect-multiplier"`
}

// detectTime estimates how long this session takes to declare the peer dead:
// the detect multiplier times the negotiated interval, which is the slower of
// our receive interval and the peer's transmit interval. Before the session
// comes up the peer has reported nothing, so our own transmit interval stands
// in for theirs.
func (p frrBFDPeer) detectTime() time.Duration {
	remoteTx := p.RemoteTransmitInterval
	if remoteTx <= 0 {
		remoteTx = p.TransmitInterval
	}
	interval := p.ReceiveInterval
	if remoteTx > interval {
		interval = remoteTx
	}
	return bfdDetectTime(p.DetectMultiplier, interval)
}

// inVRF reports whether the peer belongs to the given VRF. Per the type's
// contract an unreported vrf means "unknown", not "some other VRF": a bfdd
// release that omits the field would otherwise make every session look foreign,
// and a node whose BFD is entirely healthy would report running none at all.
func (p frrBFDPeer) inVRF(vrf string) bool {
	return p.VRF == "" || p.VRF == vrf
}

// ListFRRBFDPeers returns the BFD sessions FRR knows about. A fabric without
// BFD — or a node where bfdd is not running — makes vtysh print a "% …"
// notice, or nothing at all, instead of JSON. For a check that is on by default
// that is an expected state, not an error, so it yields no peers.
//
// Output that is neither is an error, not an empty peer list: reading it as "no
// BFD anywhere" would flip the gauge to +Inf and page an operator over a fabric
// whose BFD is healthy.
func (rm *RouteManager) ListFRRBFDPeers() ([]frrBFDPeer, error) {
	output, err := rm.runVtysh("-c", "show bfd peers json")
	if err != nil {
		return nil, fmt.Errorf("vtysh show bfd peers: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || strings.HasPrefix(trimmed, "%") {
		slog.Debug("vtysh returned no BFD peer JSON", "output", trimmed)
		return nil, nil
	}
	var peers []frrBFDPeer
	if err := json.Unmarshal([]byte(trimmed), &peers); err != nil {
		return nil, fmt.Errorf("parse vtysh bfd peers json: %w", err)
	}
	return peers, nil
}

// worstPeerDetectTime returns the longest detection time across the BFD
// sessions in the given VRF, and the peer it belongs to.
func worstPeerDetectTime(peers []frrBFDPeer, vrf string) (time.Duration, string) {
	var worst time.Duration
	var name string
	for _, p := range peers {
		if !p.inVRF(vrf) {
			continue
		}
		if d := p.detectTime(); d > worst {
			worst, name = d, p.Peer
		}
	}
	return worst, name
}

// =============================================================================
// Reconcile integration
// =============================================================================

// reportBFDUnknown records that a layer's BFD state could not be read. The
// gauge goes to NaN rather than 0: a layer the agent cannot see has an unknown
// detection time, and 0 is the healthiest value the gauge can take — every
// threshold alert would go quiet on the one reading that means "I do not know".
// The error counter carries the signal that NaN cannot.
func reportBFDUnknown(layer string) {
	setBFDDetectSeconds(layer, math.NaN())
	recordBFDCheckError(layer)
}

// reconcileBFD estimates the BFD failure-detection time per layer, exports it
// and warns when it exceeds bfd_check_max_detect. It runs at the end of a
// reconcile cycle so it never delays route programming on the failover hot
// path, and every failure is logged rather than aborting the cycle.
func (a *Agent) reconcileBFD() {
	// There is no OVS — and so no Geneve tunnel — in port-forward-only mode.
	// FRR still announces the VIP /32s there, so its layer always runs.
	if !a.cfg.PortForwardOnly {
		a.reconcileOVNBFD()
	}
	a.reconcileFRRBFD()
}

func (a *Agent) reconcileOVNBFD() {
	if !a.cfg.BFDCheckEnabled && !a.cfg.OVNBFDManage {
		return
	}
	tunnels, err := a.routing.GeneveTunnels()
	if err != nil {
		slog.Warn("failed to enumerate Geneve tunnels for the BFD check", "error", err)
		if a.cfg.BFDCheckEnabled {
			reportBFDUnknown("ovn")
		}
		return
	}

	if a.cfg.OVNBFDManage {
		if err := a.routing.EnsureOVNBFDTimers(tunnels, a.cfg.OVNBFDMinRxMs, a.cfg.OVNBFDMinTxMs); err != nil {
			slog.Warn("failed to set BFD timers on Geneve tunnels", "error", err)
		}
	}

	if !a.cfg.BFDCheckEnabled {
		return
	}
	// The estimate comes from the bfd column as OVSDB reported it above, never
	// from the values just written: an ovn-controller that reclaims the column
	// would otherwise be invisible to the very check meant to prove that
	// ovn_bfd_manage took effect.
	worst, tunnel := worstTunnelDetectTime(tunnels)
	if worst == 0 {
		// No tunnel runs a BFD session, so nothing bounds how long OVN takes to
		// notice a dead chassis. That is the worst state this check can find —
		// it must not share the gauge value of the best one.
		setBFDDetectSeconds("ovn", math.Inf(1))
		slog.Warn("no Geneve tunnel runs a BFD session; OVN's gateway failover is not bounded by BFD", "layer", "ovn")
		return
	}
	setBFDDetectSeconds("ovn", worst.Seconds())
	if worst > a.cfg.BFDCheckMaxDetect {
		slog.Warn("estimated BFD detection time exceeds bfd_check_max_detect",
			"layer", "ovn", "estimate", worst, "max_detect", a.cfg.BFDCheckMaxDetect, "tunnel", tunnel)
	}
}

func (a *Agent) reconcileFRRBFD() {
	if !a.cfg.BFDCheckEnabled {
		return
	}
	peers, err := a.routing.ListFRRBFDPeers()
	if err != nil {
		slog.Warn("failed to list FRR BFD peers for the BFD check", "error", err)
		reportBFDUnknown("frr")
		return
	}

	worst, peer := worstPeerDetectTime(peers, a.cfg.VRFName)
	if worst == 0 {
		// No BFD session exists in this VRF, so the fabric withdraws a crashed
		// node's /32s only when the BGP hold timer expires — the state every
		// node is in before frr_bfd_manage is enabled, and the one an operator
		// most needs to see.
		setBFDDetectSeconds("frr", math.Inf(1))
		slog.Warn("no BGP neighbor in the VRF runs a BFD session; route withdrawal is bounded only by the BGP hold timer",
			"layer", "frr", "vrf", a.cfg.VRFName)
		return
	}
	setBFDDetectSeconds("frr", worst.Seconds())
	if worst > a.cfg.BFDCheckMaxDetect {
		slog.Warn("estimated BFD detection time exceeds bfd_check_max_detect",
			"layer", "frr", "estimate", worst, "max_detect", a.cfg.BFDCheckMaxDetect, "peer", peer)
	}
}
