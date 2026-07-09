package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
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
	Interface              string `json:"interface"`
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

// frrBFDProfileName is the bfd profile the agent owns. Neighbors are attached
// to it by name so its timers can be retuned in one place.
const frrBFDProfileName = "ovn-network-agent"

// isValidBGPNeighbor guards the neighbor identifier before it is interpolated
// into a vtysh command. FRR identifies a neighbor by IP address, or by
// interface name for unnumbered peering.
func isValidBGPNeighbor(addr string) bool {
	if net.ParseIP(addr) != nil {
		return true
	}
	return isValidIdentifier(addr)
}

// VRFBGPNeighbors returns the addresses of the VRF's configured BGP neighbors,
// sorted. A VRF with no BGP instance makes vtysh print a "% …" notice, or
// nothing at all, instead of JSON, which yields no neighbors rather than an
// error. Output that is neither is an error: reading it as "no neighbors" would
// silently disable BFD management on a VRF that has peering.
//
// Only the object keys are read. Whether a neighbor already carries the agent's
// bfd profile is decided from the running configuration instead — see
// parseFRRBGPInstance.
//
// Safety: vrfName is validated by isValidIdentifier in config validation, and
// every neighbor key is validated by isValidBGPNeighbor before use.
func (rm *RouteManager) VRFBGPNeighbors() ([]string, error) {
	output, err := rm.runVtysh("-c", fmt.Sprintf("show bgp vrf %s neighbors json", rm.vrfName))
	if err != nil {
		return nil, fmt.Errorf("vtysh show bgp neighbors: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" || strings.HasPrefix(trimmed, "%") {
		slog.Debug("vtysh returned no BGP neighbor JSON", "vrf", rm.vrfName, "output", trimmed)
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return nil, fmt.Errorf("parse vtysh bgp neighbors json: %w", err)
	}

	var neighbors []string
	for addr := range raw {
		if !isValidBGPNeighbor(addr) {
			slog.Warn("skipping BGP neighbor with an unexpected identifier", "neighbor", addr, "vrf", rm.vrfName)
			continue
		}
		neighbors = append(neighbors, addr)
	}
	// Walk the neighbors in a stable order so the emitted command sequence does
	// not depend on Go's map iteration order.
	sort.Strings(neighbors)
	return neighbors, nil
}

// frrBGPInstance is the VRF's `router bgp` stanza as it stands in FRR's running
// configuration.
type frrBGPInstance struct {
	// AS is the number the stanza is keyed on. 0 means the VRF has no BGP
	// instance: `router bgp <as> vrf <vrf>` is a create-or-enter command, and
	// the agent's contract is to add the bfd knob to peering an operator
	// declared — never to instantiate a BGP instance itself.
	//
	// A neighbor's reported localAs is not a substitute: `neighbor <addr>
	// local-as <asn>` overrides it per session — a routine pattern for AS
	// migration — and entering `router bgp` with the wrong AS makes FRR reject
	// the block and run the remaining commands in the wrong CLI node.
	AS int64
	// Profiled holds the neighbors the stanza already attaches to the agent's
	// bfd profile.
	Profiled map[string]bool
}

// parseFRRBGPInstance extracts the VRF's `router bgp` stanza from an FRR running
// configuration. An AS written in asdot notation (`1.10`) does not parse and
// yields AS 0, so the agent declines to configure rather than emit a command FRR
// would reject.
//
// The stanza's own neighbor lines are the authority on which neighbors carry the
// agent's profile. `show bgp … neighbors json` reports that a neighbor has BFD,
// but a neighbor can have BFD and no profile — an operator's plain `neighbor
// <addr> bfd`, or a vtysh the command timeout killed between the two commands
// that attach it. Such a neighbor runs at bgpd's session defaults, and treating
// it as configured would leave it there forever. The key an attached profile is
// reported under has also changed across FRR releases, whereas `neighbor <addr>
// bfd profile <name>` is the line the agent writes and FRR renders back verbatim.
func parseFRRBGPInstance(runningConfig, vrf string) frrBGPInstance {
	instance := frrBGPInstance{Profiled: make(map[string]bool)}
	const header = "router bgp "
	vrfSuffix := " vrf " + vrf
	profileSuffix := " bfd profile " + frrBFDProfileName

	inStanza := false
	for _, raw := range strings.Split(runningConfig, "\n") {
		line := strings.TrimSpace(raw)
		// The stanza body is indented; `exit`, `!` and the next `router bgp` all
		// start at column 0.
		if inStanza && (line == "" || raw != line) {
			if addr, ok := strings.CutPrefix(line, "neighbor "); ok {
				if addr, ok := strings.CutSuffix(addr, profileSuffix); ok {
					instance.Profiled[addr] = true
				}
			}
			continue
		}
		inStanza = false
		if !strings.HasPrefix(line, header) || !strings.HasSuffix(line, vrfSuffix) {
			continue
		}
		as, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(line, header), vrfSuffix), 10, 64)
		if err != nil || as <= 0 {
			continue
		}
		instance.AS = as
		inStanza = true
	}
	return instance
}

// frrBFDProfileConfigured reports whether the agent's bfd profile still exists
// in FRR's running configuration.
//
// FRR holds the profile only in its running state. A `systemctl reload frr` or
// an frr-reload.py run reapplies frr.conf, which carries the agent's `neighbor
// <addr> bfd profile <name>` lines once an operator has written them to memory,
// but not the profile stanza the agent never persisted. The neighbors then
// reference a profile that no longer exists, bfdd silently falls back to its own
// defaults, and every neighbor still reports BFD — so nothing else notices.
func frrBFDProfileConfigured(runningConfig string) bool {
	want := "profile " + frrBFDProfileName
	for _, line := range strings.Split(runningConfig, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// frrRunningConfig returns FRR's running configuration.
func (rm *RouteManager) frrRunningConfig() (string, error) {
	output, err := rm.runVtysh("-c", "show running-config")
	if err != nil {
		return "", fmt.Errorf("vtysh show running-config: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

// applyFRRBFDProfile writes the agent's bfd profile, which carries the timers
// every attached neighbor then uses.
func (rm *RouteManager) applyFRRBFDProfile(minRxMs, minTxMs, multiplier int) error {
	output, err := rm.runVtysh(
		"-c", "conf t",
		"-c", "bfd",
		"-c", fmt.Sprintf("profile %s", frrBFDProfileName),
		"-c", fmt.Sprintf("receive-interval %d", minRxMs),
		"-c", fmt.Sprintf("transmit-interval %d", minTxMs),
		"-c", fmt.Sprintf("detect-multiplier %d", multiplier),
		"-c", "exit",
		"-c", "exit",
		"-c", "end")
	if err != nil {
		return fmt.Errorf("vtysh write bfd profile: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// attachFRRBFDNeighbor enables BFD on one BGP neighbor and points it at the
// agent's profile.
//
// One vtysh invocation per neighbor, not one batch for all of them: vtysh stops
// at the first command FRR rejects, so a single neighbor bgpd refuses to
// configure — a dynamic peer from `bgp listen range`, which `show bgp …
// neighbors json` reports but no `neighbor <addr>` line configures — would keep
// every neighbor sorted after it from ever getting BFD, and leave the agent
// unable to tell which of them had already been attached. A neighbor is attached
// once per process, so the extra invocations are paid on the first reconcile.
//
// Safety: addr is validated by isValidBGPNeighbor before it reaches here.
func (rm *RouteManager) attachFRRBFDNeighbor(as int64, addr string) error {
	output, err := rm.runVtysh(
		"-c", "conf t",
		"-c", fmt.Sprintf("router bgp %d vrf %s", as, rm.vrfName),
		"-c", fmt.Sprintf("neighbor %s bfd", addr),
		"-c", fmt.Sprintf("neighbor %s bfd profile %s", addr, frrBFDProfileName),
		"-c", "end")
	if err != nil {
		return fmt.Errorf("vtysh enable BFD on neighbor %s: %w (output: %s)", addr, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// EnsureFRRBFD enables BFD on the VRF's already-configured BGP neighbors,
// attaching each to the agent's bfd profile, which carries the timers. Only
// neighbors the running configuration does not already attach to that profile
// are configured, so once the fleet has settled a reconcile issues no vtysh
// configuration at all. That matters: every `conf t` takes FRR's configuration
// lock, and re-issuing `neighbor <addr> bfd` can reinstall the session —
// bouncing the very fabric BFD session this feature exists to keep up.
//
// Nothing guarantees that a neighbor `show bgp … neighbors json` reports can be
// rendered back as `neighbor <addr> bfd profile <name>`: a peer-group member
// whose BFD FRR writes under the group never appears attached, however often it
// is configured. Such a neighbor would keep the set non-empty forever, so each
// neighbor is attached at most once per process — frrBFDAttached remembers the
// attempt, and a neighbor still unattached on the next reconcile is reported and
// left alone rather than reconfigured every 60 s for the life of the node.
//
// The profile is written on the first reconcile of the process and whenever
// FRR's running configuration no longer carries it. The first case applies a
// changed timer configuration, which the agent only ever reads at startup; the
// second restores a profile an frr-reload dropped out from under the neighbors
// still pointing at it.
//
// The running configuration is read once per call and answers all three
// questions — profile present, instance AS, neighbors already attached — from
// one snapshot. `router bgp <as> vrf <vrf>` is still create-or-enter, so an
// operator who deletes the instance between that read and the write below has a
// window in which the agent recreates it; one read makes the window as small as
// two separate vtysh processes allow.
//
// This closes the "stale /32 from the dead node" gap: without BFD the fabric
// keeps the crashed node's routes until the BGP hold timer expires. The fabric
// side must enable BFD too — see docs/explanation/bfd-failover-detection.md.
func (rm *RouteManager) EnsureFRRBFD(minRxMs, minTxMs, multiplier int) error {
	if rm.dryRun {
		slog.Info("[dry-run] would enable BFD on the VRF's BGP neighbors",
			"vrf", rm.vrfName, "min_rx_ms", minRxMs, "min_tx_ms", minTxMs, "multiplier", multiplier)
		return nil
	}

	neighbors, err := rm.VRFBGPNeighbors()
	if err != nil {
		return err
	}
	if len(neighbors) == 0 {
		return nil // no BGP peering in this VRF — nothing to attach a profile to
	}

	runningConfig, err := rm.frrRunningConfig()
	if err != nil {
		return err
	}
	instance := parseFRRBGPInstance(runningConfig, rm.vrfName)

	var needing, unrendered []string
	for _, addr := range neighbors {
		switch {
		case instance.Profiled[addr]:
			// An operator who removed the line gets it back on the next cycle.
			delete(rm.frrBFDAttached, addr)
		case rm.frrBFDAttached[addr]:
			unrendered = append(unrendered, addr)
		default:
			needing = append(needing, addr)
		}
	}
	if len(unrendered) > 0 {
		slog.Warn("BGP neighbors took the bfd profile but FRR does not render it back; not attaching them again",
			"vrf", rm.vrfName, "neighbors", strings.Join(unrendered, " "))
	}
	if len(needing) > 0 && instance.AS == 0 {
		return fmt.Errorf("cannot configure BFD: no router bgp instance for vrf %s", rm.vrfName)
	}

	if !rm.frrBFDProfileApplied || !frrBFDProfileConfigured(runningConfig) {
		if err := rm.applyFRRBFDProfile(minRxMs, minTxMs, multiplier); err != nil {
			return err
		}
		rm.frrBFDProfileApplied = true
	}
	if len(needing) == 0 {
		return nil
	}
	if rm.frrBFDAttached == nil {
		rm.frrBFDAttached = make(map[string]bool, len(needing))
	}

	var failed []string
	for _, addr := range needing {
		if err := rm.attachFRRBFDNeighbor(instance.AS, addr); err != nil {
			// A neighbor FRR rejects installs nothing, so retrying it costs one
			// vtysh invocation and bounces no session. Only a neighbor the
			// command succeeded on is remembered, so a vtysh the configuration
			// lock defeated is tried again.
			slog.Warn("failed to enable BFD on a BGP neighbor", "vrf", rm.vrfName, "neighbor", addr, "error", err)
			failed = append(failed, addr)
			continue
		}
		rm.frrBFDAttached[addr] = true
	}
	if len(failed) > 0 {
		return fmt.Errorf("enable BFD on bgp neighbors %s of vrf %s", strings.Join(failed, " "), rm.vrfName)
	}
	slog.Info("BFD profile applied to the VRF's BGP neighbors",
		"vrf", rm.vrfName, "attached", len(needing), "min_rx_ms", minRxMs, "min_tx_ms", minTxMs, "multiplier", multiplier)
	return nil
}

// neighborsWithoutBFD returns the VRF's BGP neighbors that run no BFD session
// at all.
//
// `show bfd peers json` lists the sessions that exist, so a neighbor that was
// never attached to a profile is simply absent and contributes nothing to
// worstPeerDetectTime. Three fast sessions and one missing one would report the
// fast estimate, while the missing neighbor still withdraws a crashed node's
// /32s only when the BGP hold timer expires — 180 s by default. That is the
// half-took-effect state frr_bfd_manage can leave behind, and precisely the one
// the check exists to catch.
//
// An unnumbered neighbor is named by its interface, which bfdd reports as
// `interface` next to the link-local `peer` address, so both identify a session.
func neighborsWithoutBFD(neighbors []string, peers []frrBFDPeer, vrf string) []string {
	sessions := make(map[string]bool, 2*len(peers))
	for _, p := range peers {
		if !p.inVRF(vrf) {
			continue
		}
		sessions[p.Peer] = true
		if p.Interface != "" {
			sessions[p.Interface] = true
		}
	}
	var missing []string
	for _, n := range neighbors {
		if !sessions[n] {
			missing = append(missing, n)
		}
	}
	return missing
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
	if !a.cfg.BFDCheckEnabled && !a.cfg.FRRBFDManage {
		return
	}

	if a.cfg.FRRBFDManage {
		if err := a.routing.EnsureFRRBFD(a.cfg.FRRBFDMinRxMs, a.cfg.FRRBFDMinTxMs, a.cfg.FRRBFDMultiplier); err != nil {
			slog.Warn("failed to enable BFD on the VRF's BGP neighbors", "error", err)
		}
	}

	if !a.cfg.BFDCheckEnabled {
		return
	}
	peers, err := a.routing.ListFRRBFDPeers()
	if err != nil {
		slog.Warn("failed to list FRR BFD peers for the BFD check", "error", err)
		reportBFDUnknown("frr")
		return
	}
	neighbors, err := a.routing.VRFBGPNeighbors()
	if err != nil {
		slog.Warn("failed to list the VRF's BGP neighbors for the BFD check", "error", err)
		reportBFDUnknown("frr")
		return
	}

	if missing := neighborsWithoutBFD(neighbors, peers, a.cfg.VRFName); len(missing) > 0 {
		// One neighbor without a session is enough: the fabric withdraws this
		// node's /32s over every session, and the slowest one bounds the
		// failover. The other sessions' timers say nothing about that neighbor.
		setBFDDetectSeconds("frr", math.Inf(1))
		slog.Warn("BGP neighbors run no BFD session; their route withdrawal is bounded only by the BGP hold timer",
			"layer", "frr", "vrf", a.cfg.VRFName, "neighbors", strings.Join(missing, " "))
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
