package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ovsExecFunc is the signature of an exec.Cmd runner. Tests inject a stub via
// RouteManager.execOVSHook to capture OVS commands without running them.
type ovsExecFunc func(*exec.Cmd) ([]byte, error)

// Veth interface names shared between routing and nftables code.
const (
	vethDefaultName  = "veth-default"
	vethProviderName = "veth-provider"
)

// RouteManager handles kernel routes on the provider bridge and FRR static routes.
type RouteManager struct {
	bridgeDev    string
	bridgeIP     string
	vrfName      string
	vethNexthop  string
	routeTableID int
	ovsWrapper   []string // prefix args for ovs-vsctl/ovs-ofctl (e.g. ["docker", "exec", "openvswitch_vswitchd"])
	dryRun       bool

	// Veth VRF leak settings
	vethLeakEnabled      bool
	vethProviderIP       string
	vethLeakTableID      int
	vethLeakRulePriority int
	networkFilters       []*net.IPNet // from manual config (may be empty for auto-discovery)

	// FRR prefix-list management
	frrPrefixList string

	// Port forwarding (DNAT) settings
	portForwardEnabled      bool
	portForwardDev          string
	portForwardTableID      int
	portForwardL3mdevAccept bool
	portForwardCTZone       int
	portForwards            []PortForwardVIP

	// segments maps each localnet port name to its resolved OVS/kernel
	// binding (populated on first use, revalidated every reconcile). The
	// "" key is the legacy single-patch-port fallback binding used when a
	// segment cannot be resolved — flat deployments, or an OVN version
	// that does not set external_ids:ovn-localnet-port on patch ports.
	segments map[string]*segmentBinding

	// execOVSHook, when non-nil, replaces the real exec.Cmd runner used by
	// OVS helpers. Tests set this to capture commands without executing them.
	execOVSHook ovsExecFunc

	// segmentIfaceHook, when non-nil, replaces EnsureSegmentInterface in
	// refreshSegmentBindings. Tests set this to resolve VLAN segments to
	// synthetic kernel interfaces without touching netlink.
	segmentIfaceHook func(tag int) (dev, mac string, err error)

	// listKernelRoutesHook, when non-nil, replaces ListKernelRoutes in the
	// agent's route reconciliation. Tests set this to inject kernel route
	// state without touching netlink.
	listKernelRoutesHook func() ([]kernelRouteEntry, error)

	// execVtyshHook, when non-nil, replaces the real exec.Cmd runner used by
	// FRR/vtysh helpers. Tests set this to capture commands without executing them.
	execVtyshHook ovsExecFunc
}

// runVtysh executes a vtysh command, dispatching through execVtyshHook when set.
func (rm *RouteManager) runVtysh(args ...string) ([]byte, error) {
	cmd := exec.Command("vtysh", args...)
	if rm.execVtyshHook != nil {
		return rm.execVtyshHook(cmd)
	}
	return cmd.CombinedOutput()
}

func NewRouteManager(cfg Config) *RouteManager {
	rm := &RouteManager{
		bridgeDev:               cfg.BridgeDev,
		bridgeIP:                cfg.BridgeIP,
		vrfName:                 cfg.VRFName,
		vethNexthop:             cfg.VethNexthop,
		routeTableID:            cfg.RouteTableID,
		dryRun:                  cfg.DryRun,
		vethLeakEnabled:         cfg.VethLeakEnabled,
		vethProviderIP:          cfg.VethProviderIP,
		vethLeakTableID:         cfg.VethLeakTableID,
		vethLeakRulePriority:    cfg.VethLeakRulePriority,
		networkFilters:          cfg.NetworkFilters,
		frrPrefixList:           cfg.FRRPrefixList,
		portForwardEnabled:      cfg.PortForwardEnabled,
		portForwardDev:          cfg.PortForwardDev,
		portForwardTableID:      cfg.PortForwardTableID,
		portForwardL3mdevAccept: cfg.PortForwardL3mdevAccept,
		portForwardCTZone:       cfg.PortForwardCTZone,
		portForwards:            cfg.PortForwards,
	}
	if cfg.OVSWrapper != "" {
		rm.ovsWrapper = strings.Fields(cfg.OVSWrapper)
	}
	return rm
}

// kernelRouteEntry is one agent-managed /32 kernel route: the destination IP
// and the kernel interface it is bound to (the bridge device or a per-VLAN
// segment interface).
type kernelRouteEntry struct {
	IP  string
	Dev string
}

// listKernelRoutes dispatches to the platform ListKernelRoutes, or to the
// test hook when one is set.
func (rm *RouteManager) listKernelRoutes() ([]kernelRouteEntry, error) {
	if rm.listKernelRoutesHook != nil {
		return rm.listKernelRoutesHook()
	}
	return rm.ListKernelRoutes()
}

// validateIP checks that the given string is a valid IPv4 address. IPv6 is
// rejected: the FRR and kernel route paths that call this are IPv4-only (vtysh
// "ip route .../32", net.CIDRMask(32, 32)), so a v6 address would produce an
// invalid command or a malformed route. Full IPv6 support is tracked in
// #85/#70.
func validateIP(ip string) error {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return fmt.Errorf("invalid IP address: %q", ip)
	}
	if parsed.To4() == nil {
		return fmt.Errorf("not an IPv4 address: %q", ip)
	}
	return nil
}

// =============================================================================
// FRR routes via vtysh
// =============================================================================

// AddFRRRoute is a convenience wrapper for AddFRRRoutes with a single IP.
func (rm *RouteManager) AddFRRRoute(ip string) error {
	return rm.AddFRRRoutes([]string{ip})
}

// frrBatchSize is the maximum number of route operations per vtysh call.
// This avoids hitting the OS ARG_MAX limit (~2 MB on Linux) when managing
// thousands of FIPs at startup.
const frrBatchSize = 500

// AddFRRRoutes adds static /32 routes for all given IPs via vtysh. Invalid
// entries are skipped with a warning instead of rejecting the whole batch, so
// one malformed OVN row degrades only its own FIP. The valid IPs are chunked
// into batches of frrBatchSize to stay within OS argument-list limits; a failed
// chunk is logged and the remaining chunks still run. The returned error is the
// join of every failed chunk (nil when every valid route applied), so a real
// vtysh command failure still makes ensureRoutes withhold the takeover marker,
// while un-installable entries are dropped without blocking the rest.
func (rm *RouteManager) AddFRRRoutes(ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	valid := make([]string, 0, len(ips))
	var skipped []string
	for _, ip := range ips {
		if err := validateIP(ip); err != nil {
			skipped = append(skipped, ip)
			continue
		}
		valid = append(valid, ip)
	}
	if len(skipped) > 0 {
		slog.Warn("skipping invalid IPs in FRR route batch", "count", len(skipped), "ips", skipped)
	}
	if len(valid) == 0 {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would add FRR routes", "count", len(valid), "vrf", rm.vrfName, "nexthop", rm.vethNexthop)
		return nil
	}
	var errs []error
	for start := 0; start < len(valid); start += frrBatchSize {
		end := start + frrBatchSize
		if end > len(valid) {
			end = len(valid)
		}
		chunk := valid[start:end]
		args := []string{"-c", "conf t", "-c", fmt.Sprintf("vrf %s", rm.vrfName)}
		for _, ip := range chunk {
			args = append(args, "-c", fmt.Sprintf("ip route %s/32 %s", ip, rm.vethNexthop))
		}
		args = append(args, "-c", "exit-vrf", "-c", "end")
		output, err := rm.runVtysh(args...)
		if err != nil {
			slog.Error("failed to add FRR route chunk, continuing with the next",
				"count", len(chunk), "vrf", rm.vrfName, "error", err, "output", strings.TrimSpace(string(output)))
			errs = append(errs, fmt.Errorf("vtysh batch add %d routes: %w (output: %s)", len(chunk), err, strings.TrimSpace(string(output))))
			continue
		}
		slog.Info("FRR routes ensured", "count", len(chunk), "vrf", rm.vrfName, "nexthop", rm.vethNexthop)
	}
	return errors.Join(errs...)
}

// DelFRRRoute is a convenience wrapper for DelFRRRoutes with a single IP.
func (rm *RouteManager) DelFRRRoute(ip string) error {
	return rm.DelFRRRoutes([]string{ip})
}

// DelFRRRoutes removes static /32 routes for all given IPs via vtysh.
// IPs are validated before any commands are executed.  The list is chunked
// into batches of frrBatchSize to stay within OS argument-list limits.
func (rm *RouteManager) DelFRRRoutes(ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	for _, ip := range ips {
		if err := validateIP(ip); err != nil {
			return err
		}
	}
	if rm.dryRun {
		slog.Info("[dry-run] would remove FRR routes", "count", len(ips), "vrf", rm.vrfName, "nexthop", rm.vethNexthop)
		return nil
	}
	for start := 0; start < len(ips); start += frrBatchSize {
		end := start + frrBatchSize
		if end > len(ips) {
			end = len(ips)
		}
		chunk := ips[start:end]
		args := []string{"-c", "conf t", "-c", fmt.Sprintf("vrf %s", rm.vrfName)}
		for _, ip := range chunk {
			args = append(args, "-c", fmt.Sprintf("no ip route %s/32 %s", ip, rm.vethNexthop))
		}
		args = append(args, "-c", "exit-vrf", "-c", "end")
		output, err := rm.runVtysh(args...)
		if err != nil {
			return fmt.Errorf("vtysh batch del %d routes: %w (output: %s)", len(chunk), err, strings.TrimSpace(string(output)))
		}
		slog.Info("FRR routes removed", "count", len(chunk), "vrf", rm.vrfName)
	}
	return nil
}

// HasFRRRoute checks if a static route for the IP exists in the VRF.
func (rm *RouteManager) HasFRRRoute(ip string) bool {
	if err := validateIP(ip); err != nil {
		return false
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip route vrf %s %s/32", rm.vrfName, ip))
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "static")
}

// ListFRRRoutes returns the agent's own static /32 routes in the VRF: those
// installed via the agent's veth nexthop. This nexthop scoping is the FRR
// analog of the kernel protocol tag — without it, reconciliation would treat
// operator-created statics as agent-owned and withdraw them on standby nodes.
func (rm *RouteManager) ListFRRRoutes() ([]string, error) {
	if rm.dryRun {
		return nil, nil
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip route vrf %s static", rm.vrfName))
	if err != nil {
		return nil, fmt.Errorf("vtysh list routes: %w", err)
	}

	var ips []string
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		// Lines like: S>* 198.51.100.10/32 [1/0] via 169.254.0.1, veth-default, ...
		if !strings.HasPrefix(line, "S") || !strings.Contains(line, "/32") {
			continue
		}
		// Only report statics installed via the agent's own nexthop.
		if frrRouteNexthop(line) != rm.vethNexthop {
			continue
		}
		for _, p := range strings.Fields(line) {
			if strings.Contains(p, "/32") {
				ip, _, _ := net.ParseCIDR(p)
				if ip != nil {
					ips = append(ips, ip.String())
				}
				break
			}
		}
	}
	return ips, nil
}

// frrRouteNexthop returns the nexthop address in an FRR "show ip route" line —
// the token following "via", with any trailing comma stripped. Returns "" when
// the line has no via clause.
func frrRouteNexthop(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			return strings.TrimRight(fields[i+1], ",")
		}
	}
	return ""
}

// frrRouteEntry is the subset of an FRR `show ip route ... json` route object
// that the agent inspects. A static route is only advertised via BGP once it
// is both selected (FRR's best route for the prefix) and installed (present in
// the kernel FIB); FRR omits these keys when false, so the zero value is the
// correct default.
type frrRouteEntry struct {
	Selected  bool `json:"selected"`
	Installed bool `json:"installed"`
}

// InactiveFRRRoutes returns, from the given /32 IPs, those whose static route
// exists in the VRF but is not selected and installed by FRR — i.e. configured
// but not actually advertised (typically an unresolvable next-hop). IPs with no
// static route at all are not reported: that case is a plain "missing route"
// handled by ensureRoutes. ListFRRRoutes cannot tell the two apart because it
// matches any configured route, which is why this distinct check exists.
func (rm *RouteManager) InactiveFRRRoutes(ips []string) ([]string, error) {
	if rm.dryRun || len(ips) == 0 {
		return nil, nil
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip route vrf %s static json", rm.vrfName))
	if err != nil {
		return nil, fmt.Errorf("vtysh list routes json: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	// FRR emits an empty body rather than "{}" when the VRF has no static
	// routes; treat that as "nothing configured".
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}
	var routes map[string][]frrRouteEntry
	if err := json.Unmarshal([]byte(trimmed), &routes); err != nil {
		return nil, fmt.Errorf("parse vtysh route json: %w", err)
	}

	var inactive []string
	for _, ip := range ips {
		entries, ok := routes[ip+"/32"]
		if !ok {
			continue // not configured at all — not this check's concern
		}
		active := false
		for _, e := range entries {
			if e.Selected && e.Installed {
				active = true
				break
			}
		}
		if !active {
			inactive = append(inactive, ip)
		}
	}
	sort.Strings(inactive)
	return inactive, nil
}

// RefreshBGP triggers an outbound BGP soft-refresh so that peers learn about
// route changes immediately instead of waiting for the MRAI timer.
func (rm *RouteManager) RefreshBGP() error {
	if rm.dryRun {
		slog.Info("[dry-run] would refresh BGP outbound")
		return nil
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("clear ip bgp vrf %s * soft out", rm.vrfName))
	if err != nil {
		return fmt.Errorf("BGP soft-refresh: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	slog.Info("BGP outbound soft-refresh triggered", "vrf", rm.vrfName)
	return nil
}

// =============================================================================
// FRR prefix-list management
// =============================================================================

// prefixListEntry represents a single entry in an FRR ip prefix-list.
type prefixListEntry struct {
	Seq     int
	Network string // e.g. "198.51.100.0/24"
}

// ListFRRPrefixListEntries returns the current "permit ... ge 32 le 32" entries
// in the configured FRR prefix-list. Returns nil if no prefix-list is configured.
//
// Safety: frrPrefixList is validated by isValidIdentifier (alphanumeric, hyphen,
// underscore, dot) in config validation. Network strings come from net.IPNet.String().
func (rm *RouteManager) ListFRRPrefixListEntries() ([]prefixListEntry, error) {
	if rm.frrPrefixList == "" {
		return nil, nil
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip prefix-list %s", rm.frrPrefixList))
	if err != nil {
		return nil, fmt.Errorf("vtysh show prefix-list %s: %w (output: %s)", rm.frrPrefixList, err, strings.TrimSpace(string(output)))
	}

	outStr := string(output)
	if strings.Contains(outStr, "Can't find") || strings.TrimSpace(outStr) == "" {
		return nil, nil
	}

	var entries []prefixListEntry
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		// Match: seq <N> permit <network> ge 32 le 32
		if len(fields) >= 8 && fields[0] == "seq" && fields[2] == "permit" &&
			fields[4] == "ge" && fields[5] == "32" && fields[6] == "le" && fields[7] == "32" {
			seq, serr := strconv.Atoi(fields[1])
			if serr != nil {
				continue
			}
			entries = append(entries, prefixListEntry{Seq: seq, Network: fields[3]})
		}
	}
	return entries, nil
}

// ReconcileFRRPrefixList ensures the managed prefix-list contains exactly one
// "permit <network> ge 32 le 32" entry per desired network.
// Pass nil to remove all managed entries (cleanup).
func (rm *RouteManager) ReconcileFRRPrefixList(networks []*net.IPNet) error {
	if rm.frrPrefixList == "" {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would reconcile FRR prefix-list", "name", rm.frrPrefixList, "networks", len(networks))
		return nil
	}

	current, err := rm.ListFRRPrefixListEntries()
	if err != nil {
		return fmt.Errorf("list prefix-list entries: %w", err)
	}

	// Build current and desired maps.
	currentByNetwork := make(map[string]int, len(current)) // network → seq
	maxSeq := 0
	for _, e := range current {
		currentByNetwork[e.Network] = e.Seq
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}

	desired := make(map[string]bool, len(networks))
	for _, n := range networks {
		desired[n.String()] = true
	}

	// Add missing entries (before removing stale ones, to avoid a window with no entries).
	for network := range desired {
		if _, exists := currentByNetwork[network]; !exists {
			maxSeq += 5
			output, err := rm.runVtysh(
				"-c", "conf t",
				"-c", fmt.Sprintf("ip prefix-list %s seq %d permit %s ge 32 le 32", rm.frrPrefixList, maxSeq, network),
				"-c", "end",
			)
			if err != nil {
				return fmt.Errorf("add prefix-list entry %s seq %d: %w (output: %s)", network, maxSeq, err, strings.TrimSpace(string(output)))
			}
			slog.Info("FRR prefix-list entry added", "name", rm.frrPrefixList, "network", network, "seq", maxSeq)
		}
	}

	// Remove stale entries.
	for network, seq := range currentByNetwork {
		if !desired[network] {
			output, err := rm.runVtysh(
				"-c", "conf t",
				"-c", fmt.Sprintf("no ip prefix-list %s seq %d permit %s ge 32 le 32", rm.frrPrefixList, seq, network),
				"-c", "end",
			)
			if err != nil {
				return fmt.Errorf("remove prefix-list entry %s seq %d: %w (output: %s)", network, seq, err, strings.TrimSpace(string(output)))
			}
			slog.Info("FRR prefix-list entry removed", "name", rm.frrPrefixList, "network", network, "seq", seq)
		}
	}

	return nil
}

// =============================================================================
// Helpers
// =============================================================================

func isNoSuchRoute(err error) bool {
	return strings.Contains(err.Error(), "no such process")
}

func isNoSuchRule(err error) bool {
	return strings.Contains(err.Error(), "no such file or directory")
}
