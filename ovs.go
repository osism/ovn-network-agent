package main

import (
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"sort"
	"strings"
)

const (
	ovsCookieMACTweak = "0x999"
	ovsCookieHairpin  = "0x998"
)

// ovsLocalnetPortKey is the external_ids key ovn-controller sets on the
// provider-bridge patch Port row, naming the localnet logical port the
// patch carries. It is how a localnet segment is mapped to its patch port.
const ovsLocalnetPortKey = "ovn-localnet-port"

// DesiredSegment identifies a localnet segment the reconcile loop wants OVS
// flows and a kernel path for. An empty LocalnetPort requests the legacy
// single-patch-port fallback (routers whose segment is unresolved).
type DesiredSegment struct {
	LocalnetPort string
	VLANTag      *int
}

// segmentBinding is the resolved data-plane state of one localnet segment:
// the OVS patch port (and its OpenFlow port number) carrying the segment's
// traffic on the provider bridge, and the kernel interface (and its MAC)
// carrying the segment's /32 routes, bridge IP, and proxy ARP.
type segmentBinding struct {
	patchPort string
	ofport    string
	kernelDev string
	kernelMAC string
	vlanTag   *int
}

// HairpinTarget describes one locally-managed IP that needs a hairpin flow:
// the MAC of the router port that owns it, and the localnet segment its
// external network is on ("" = the fallback segment).
type HairpinTarget struct {
	RouterMAC string
	Segment   string
}

// MACTweakFlow returns the OpenFlow rule string for a MAC-tweak flow.
func MACTweakFlow(cookie, ofport, mac string, ipv6 bool) string {
	proto := "ip"
	if ipv6 {
		proto = "ipv6"
	}
	return fmt.Sprintf("cookie=%s,priority=900,%s,in_port=%s,actions=mod_dl_dst:%s,NORMAL",
		cookie, proto, ofport, mac)
}

// ovsCmd builds an exec.Cmd for an OVS command, prepending the configured
// wrapper (e.g. "docker exec openvswitch_vswitchd") if set.
func (rm *RouteManager) ovsCmd(binary string, args ...string) *exec.Cmd {
	if len(rm.ovsWrapper) > 0 {
		fullArgs := append(rm.ovsWrapper[1:], binary)
		fullArgs = append(fullArgs, args...)
		return exec.Command(rm.ovsWrapper[0], fullArgs...)
	}
	return exec.Command(binary, args...)
}

// runOVS builds and runs an OVS command. When execOVSHook is set (tests) the
// command is dispatched through it instead of being executed.
func (rm *RouteManager) runOVS(binary string, args ...string) ([]byte, error) {
	cmd := rm.ovsCmd(binary, args...)
	if rm.execOVSHook != nil {
		return rm.execOVSHook(cmd)
	}
	return cmd.CombinedOutput()
}

// EnsureSegments resolves the desired localnet segments to their patch ports
// and kernel interfaces, prunes kernel interfaces of segments that are gone,
// and installs MAC-tweak flows per patch port. Each flow rewrites the
// destination MAC of packets arriving from OVN's integration bridge (via the
// segment's patch port) to the MAC of that segment's kernel interface, so
// the kernel can properly receive and route them.
//
// Every binding is re-validated on every call via refreshSegmentBindings.
func (rm *RouteManager) EnsureSegments(desired []DesiredSegment) error {
	if rm.dryRun {
		slog.Info("[dry-run] would ensure OVS MAC-tweak flows", "dev", rm.bridgeDev, "segments", len(desired))
		return nil
	}

	if err := rm.refreshSegmentBindings(desired); err != nil {
		return err
	}

	// Remove agent-created VLAN interfaces whose segment is no longer
	// desired, so a failover or network removal does not leave a stale
	// kernel path behind.
	keepTags := make(map[int]bool)
	for _, b := range rm.segments {
		if b.vlanTag != nil {
			keepTags[*b.vlanTag] = true
		}
	}
	if err := rm.PruneSegmentInterfaces(keepTags); err != nil {
		slog.Warn("failed to prune stale segment interfaces", "error", err)
	}

	// Delete existing agent-managed flows (idempotent replace).
	if out, err := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieMACTweak)); err != nil {
		slog.Warn("failed to delete old OVS flows", "error", err, "output", strings.TrimSpace(string(out)))
	}

	// Add IPv4 and IPv6 MAC-tweak flows per patch port. Several segments
	// may share one binding (fallback), so dedup by ofport.
	seenOfports := make(map[string]bool, len(rm.segments))
	for _, key := range sortedSegmentKeys(rm.segments) {
		b := rm.segments[key]
		if seenOfports[b.ofport] {
			continue
		}
		seenOfports[b.ofport] = true

		// Install both flows best-effort: the up-front del-flows already
		// swept every segment's old flows, so a failed re-add on one patch
		// port must not abort the loop and leave the remaining segments
		// without any MAC-tweak flow at all. Log and move on.
		ipv4Flow := MACTweakFlow(ovsCookieMACTweak, b.ofport, b.kernelMAC, false)
		if err := rm.addOVSFlow(ipv4Flow); err != nil {
			slog.Warn("failed to add IPv4 MAC-tweak flow, skipping",
				"patch_port", b.patchPort, "ofport", b.ofport, "error", err)
		}

		ipv6Flow := MACTweakFlow(ovsCookieMACTweak, b.ofport, b.kernelMAC, true)
		if err := rm.addOVSFlow(ipv6Flow); err != nil {
			slog.Warn("failed to add IPv6 MAC-tweak flow, skipping",
				"patch_port", b.patchPort, "ofport", b.ofport, "error", err)
		}
	}

	slog.Debug("OVS MAC-tweak flows ensured", "dev", rm.bridgeDev, "segments", len(rm.segments))
	return nil
}

// sortedSegmentKeys returns the segment map keys in deterministic order.
func sortedSegmentKeys(segments map[string]*segmentBinding) []string {
	keys := make([]string, 0, len(segments))
	for k := range segments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// refreshSegmentBindings makes sure rm.segments reflects the current br-ex
// state for exactly the desired segments.
//
// The discovery results cannot be cached for the process lifetime:
// ovn-controller can delete and recreate a patch port — on a bridge-mapping
// change, an OVS resync, or its own restart — which assigns the port a new
// OpenFlow port number. A stale cached ofport makes every MAC-tweak and
// hairpin flow match a dead in_port, silently breaking the OVN↔kernel data
// path until the agent is restarted.
//
// The fast path keeps the cache whenever every desired segment already has a
// binding whose patch port still resolves to the cached ofport; otherwise
// (first call, segment set changed, or a port was recreated) it rediscovers
// from scratch.
func (rm *RouteManager) refreshSegmentBindings(desired []DesiredSegment) error {
	if rm.segmentsCurrent(desired) {
		return nil
	}
	// Drop the stale bindings up front: if rediscovery aborts half-way, the
	// hairpin reconcile must see "no bindings" rather than reuse dead
	// ofports.
	rm.segments = nil

	// Map localnet port name → provider-bridge patch port, from the
	// external_ids ovn-controller stamps on the patch Port rows.
	out, err := rm.runOVS("ovs-vsctl", "list-ports", rm.bridgeDev)
	if err != nil {
		return fmt.Errorf("list-ports %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}
	patchByLocalnet := make(map[string]string)
	for _, port := range strings.Fields(strings.TrimSpace(string(out))) {
		val, err := rm.runOVS("ovs-vsctl", "--if-exists", "get", "Port", port,
			"external_ids:"+ovsLocalnetPortKey)
		if err != nil {
			continue
		}
		name := strings.Trim(strings.TrimSpace(string(val)), "\"")
		if name != "" {
			patchByLocalnet[name] = port
		}
	}

	segments := make(map[string]*segmentBinding, len(desired))
	var fallback *segmentBinding
	for _, d := range desired {
		patchPort := ""
		if d.LocalnetPort != "" {
			patchPort = patchByLocalnet[d.LocalnetPort]
		}
		if patchPort == "" {
			// Legacy fallback: first patch port on the bridge, kernel path
			// on the bridge device itself. Keeps single-network flat
			// deployments bit-compatible with the pre-segment behavior.
			if d.LocalnetPort != "" {
				slog.Warn("localnet segment has no matching patch port on the provider bridge, using single-patch fallback",
					"localnet_port", d.LocalnetPort, "dev", rm.bridgeDev)
			}
			if fallback == nil {
				fallback, err = rm.fallbackBinding()
				if err != nil {
					return err
				}
			}
			segments[d.LocalnetPort] = fallback
			continue
		}

		ofport, err := rm.getOFPort(patchPort)
		if err != nil {
			return fmt.Errorf("get ofport for %s: %w", patchPort, err)
		}
		binding := &segmentBinding{patchPort: patchPort, ofport: ofport}
		if d.VLANTag == nil {
			// Flat segment: the kernel path lives on the bridge device.
			mac, err := rm.GetBridgeMAC()
			if err != nil {
				return fmt.Errorf("get bridge MAC: %w", err)
			}
			binding.kernelDev = rm.bridgeDev
			binding.kernelMAC = mac.String()
		} else {
			tag := *d.VLANTag
			dev, mac, err := rm.ensureSegmentInterface(tag)
			if err != nil {
				slog.Error("cannot ensure kernel interface for VLAN segment, skipping segment",
					"localnet_port", d.LocalnetPort, "tag", tag, "error", err)
				continue
			}
			binding.kernelDev = dev
			binding.kernelMAC = mac
			binding.vlanTag = &tag
		}
		segments[d.LocalnetPort] = binding
		slog.Info("localnet segment bound",
			"localnet_port", d.LocalnetPort,
			"patch_port", binding.patchPort,
			"ofport", binding.ofport,
			"kernel_dev", binding.kernelDev,
			"mac", binding.kernelMAC,
		)
	}

	rm.segments = segments
	return nil
}

// segmentsCurrent reports whether the cached bindings cover exactly the
// desired segments (same keys, same VLAN tags) and every cached patch port
// still resolves to its cached ofport.
func (rm *RouteManager) segmentsCurrent(desired []DesiredSegment) bool {
	if len(rm.segments) == 0 {
		return false
	}
	byKey := make(map[string]DesiredSegment, len(desired))
	for _, d := range desired {
		byKey[d.LocalnetPort] = d
	}
	if len(byKey) != len(rm.segments) {
		return false
	}
	for key, d := range byKey {
		b, ok := rm.segments[key]
		if !ok || !tagsEqual(b.vlanTag, d.VLANTag) {
			return false
		}
		if ofport, err := rm.getOFPort(b.patchPort); err != nil || ofport != b.ofport {
			slog.Info("segment patch port changed or gone, rediscovering OVS flow targets",
				"localnet_port", key, "cached_patch_port", b.patchPort, "cached_ofport", b.ofport)
			return false
		}
	}
	return true
}

// tagsEqual compares two optional VLAN tags. A binding that fell back to the
// single-patch path has a nil tag regardless of the desired tag, so a
// still-unresolvable VLAN segment keeps re-triggering rediscovery until its
// patch port appears — which is intended.
func tagsEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ensureSegmentInterface dispatches to the platform implementation, or to
// the test hook when one is set.
func (rm *RouteManager) ensureSegmentInterface(tag int) (string, string, error) {
	if rm.segmentIfaceHook != nil {
		return rm.segmentIfaceHook(tag)
	}
	return rm.EnsureSegmentInterface(tag)
}

// fallbackBinding resolves the legacy single-patch-port binding: the first
// type=patch port on the bridge device, with the kernel path on the bridge
// device itself. Used for segments that cannot be mapped to a patch port.
func (rm *RouteManager) fallbackBinding() (*segmentBinding, error) {
	patchPort, err := rm.discoverPatchPort()
	if err != nil {
		return nil, fmt.Errorf("discover patch port on %s: %w", rm.bridgeDev, err)
	}
	ofport, err := rm.getOFPort(patchPort)
	if err != nil {
		return nil, fmt.Errorf("get ofport for %s: %w", patchPort, err)
	}
	mac, err := rm.GetBridgeMAC()
	if err != nil {
		return nil, fmt.Errorf("get bridge MAC: %w", err)
	}
	return &segmentBinding{
		patchPort: patchPort,
		ofport:    ofport,
		kernelDev: rm.bridgeDev,
		kernelMAC: mac.String(),
	}, nil
}

// segmentBinding returns the binding for a localnet port name, falling back
// to the legacy "" binding when the segment is unknown. Returns nil when
// neither exists.
func (rm *RouteManager) segmentBindingFor(localnetPort string) *segmentBinding {
	if b, ok := rm.segments[localnetPort]; ok {
		return b
	}
	return rm.segments[""]
}

// SegmentDev returns the kernel interface that carries the given localnet
// segment's /32 routes, falling back to the provider bridge device when the
// segment (and the fallback binding) is not yet discovered.
func (rm *RouteManager) SegmentDev(localnetPort string) string {
	if b := rm.segmentBindingFor(localnetPort); b != nil {
		return b.kernelDev
	}
	return rm.bridgeDev
}

// SegmentMAC returns the MAC of the given localnet segment's kernel
// interface, or "" when the segment (and the fallback binding) is not yet
// discovered.
func (rm *RouteManager) SegmentMAC(localnetPort string) string {
	if b := rm.segmentBindingFor(localnetPort); b != nil {
		return b.kernelMAC
	}
	return ""
}

// HairpinFlow returns the OpenFlow rule string for a same-chassis hairpin flow.
// The flow intercepts packets from OVN (via the patch port) destined for a
// locally-managed IP and sends them back through the same patch port using
// output:in_port. OVN then processes the packet as incoming on the external
// logical switch, allowing correct DNAT/ICMP handling without leaving the host.
//
// Both source and destination MACs are rewritten:
//   - dl_src is set to the bridge device's own MAC (bridgeMAC) so the reflected
//     packet appears as external traffic to OVN, avoiding loop detection.
//   - dl_dst is set to the owning router port's MAC (routerMAC) so OVN's L2
//     lookup on the external logical switch delivers the packet to the correct
//     router. Without this, the original dl_dst may be unresolved (e.g.
//     00:00:00:00:00:00) when OVN's ARP resolution between co-located routers
//     has not completed.
//
// Priority 910 ensures hairpin fires before the MAC-tweak flow (priority 900),
// so locally-managed IPs are reflected into OVN while all other traffic
// (destined for remote IPs) still falls through to MAC-tweak and exits to the
// physical network normally.
func HairpinFlow(cookie, ofport, ip, bridgeMAC, routerMAC string, ipv6 bool) string {
	if ipv6 {
		return fmt.Sprintf("cookie=%s,priority=910,ipv6,in_port=%s,ipv6_dst=%s/128,actions=mod_dl_src:%s,mod_dl_dst:%s,output:in_port",
			cookie, ofport, ip, bridgeMAC, routerMAC)
	}
	return fmt.Sprintf("cookie=%s,priority=910,ip,in_port=%s,ip_dst=%s/32,actions=mod_dl_src:%s,mod_dl_dst:%s,output:in_port",
		cookie, ofport, ip, bridgeMAC, routerMAC)
}

// ReconcileOVSHairpinFlows installs per-IP hairpin flows on the bridge device.
//
// Without these flows, same-chassis traffic between FIPs on different OVN
// routers is mishandled: OVN sends it via the localnet port to br-ex, the
// MAC-tweak flow delivers it to the kernel, but the kernel has no "local"
// address for the destination FIP and either drops or loops the packet.
// From a different chassis the same traffic arrives via the physical network
// and OVN processes it correctly — explaining the asymmetric failure.
//
// targets maps each IP to the MAC of the router port that owns it and the
// localnet segment its external network is on. The MAC is written into the
// flow as mod_dl_dst so that OVN's L2 lookup delivers the reflected packet
// to the correct router port; the segment selects the patch port the flow
// matches on, so reflection (output:in_port) stays within the segment.
//
// EnsureSegments must be called before this method so that the segment
// bindings are populated. With no bindings this method is a no-op.
//
// Pass nil or an empty map to remove all hairpin flows (e.g. when no
// locally-active routers remain).
func (rm *RouteManager) ReconcileOVSHairpinFlows(targets map[string]HairpinTarget) error {
	if rm.dryRun {
		slog.Info("[dry-run] would reconcile OVS hairpin flows", "count", len(targets))
		return nil
	}
	if len(rm.segments) == 0 {
		// Segments not yet discovered; EnsureSegments must run first.
		slog.Warn("skipping OVS hairpin flow reconcile: segment bindings not yet discovered")
		return nil
	}

	// Full replace: delete all current hairpin flows then reinstall.
	// The replacement window is sub-millisecond and tolerable.
	if out, err := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieHairpin)); err != nil {
		return fmt.Errorf("del hairpin OVS flows on %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}

	for ip, target := range targets {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return fmt.Errorf("invalid IP %q", ip)
		}
		b := rm.segmentBindingFor(target.Segment)
		if b == nil {
			slog.Warn("no segment binding for hairpin IP, skipping",
				"ip", ip, "segment", target.Segment)
			continue
		}
		isIPv6 := parsed.To4() == nil
		flow := HairpinFlow(ovsCookieHairpin, b.ofport, ip, b.kernelMAC, target.RouterMAC, isIPv6)
		if err := rm.addOVSFlow(flow); err != nil {
			return fmt.Errorf("add hairpin flow for %s: %w", ip, err)
		}
	}

	slog.Debug("OVS hairpin flows reconciled", "count", len(targets))
	return nil
}

// RemoveOVSFlows removes all agent-managed OVS flows from the bridge device.
func (rm *RouteManager) RemoveOVSFlows() error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove OVS MAC-tweak flows", "dev", rm.bridgeDev)
		return nil
	}
	out, err := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieMACTweak))
	if err != nil {
		return fmt.Errorf("del OVS flows on %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}
	slog.Info("OVS MAC-tweak flows removed", "dev", rm.bridgeDev)

	hout, herr := rm.runOVS("ovs-ofctl", "del-flows", rm.bridgeDev,
		fmt.Sprintf("cookie=%s/-1", ovsCookieHairpin))
	if herr != nil {
		return fmt.Errorf("del hairpin OVS flows on %s: %w (output: %s)", rm.bridgeDev, herr, strings.TrimSpace(string(hout)))
	}
	slog.Info("OVS hairpin flows removed", "dev", rm.bridgeDev)

	return nil
}

// discoverPatchPort finds the patch-type port on the bridge device that
// connects to OVN's integration bridge.
func (rm *RouteManager) discoverPatchPort() (string, error) {
	out, err := rm.runOVS("ovs-vsctl", "list-ports", rm.bridgeDev)
	if err != nil {
		return "", fmt.Errorf("list-ports %s: %w (output: %s)", rm.bridgeDev, err, strings.TrimSpace(string(out)))
	}

	ports := strings.Fields(strings.TrimSpace(string(out)))
	for _, port := range ports {
		typeOut, err := rm.runOVS("ovs-vsctl", "--if-exists", "get", "Interface", port, "type")
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(typeOut)) == "patch" {
			return port, nil
		}
	}

	return "", fmt.Errorf("no patch port found on bridge %s", rm.bridgeDev)
}

// getOFPort returns the OpenFlow port number for an OVS port.
func (rm *RouteManager) getOFPort(port string) (string, error) {
	out, err := rm.runOVS("ovs-vsctl", "get", "Interface", port, "ofport")
	if err != nil {
		return "", fmt.Errorf("get ofport for %s: %w (output: %s)", port, err, strings.TrimSpace(string(out)))
	}
	ofport := strings.TrimSpace(string(out))
	if ofport == "" || ofport == "-1" {
		return "", fmt.Errorf("invalid ofport %q for %s", ofport, port)
	}
	return ofport, nil
}

func (rm *RouteManager) addOVSFlow(flow string) error {
	out, err := rm.runOVS("ovs-ofctl", "add-flow", rm.bridgeDev, flow)
	if err != nil {
		return fmt.Errorf("ovs-ofctl add-flow %s %q: %w (output: %s)", rm.bridgeDev, flow, err, strings.TrimSpace(string(out)))
	}
	return nil
}
