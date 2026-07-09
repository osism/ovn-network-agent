package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// vtyshCommandTimeout bounds a single vtysh invocation. vtysh blocks on FRR's
// configuration lock, which a concurrent frr-reload or a bgpd that stopped
// reading its vty socket can hold indefinitely. Reconciliation runs on the
// agent's only loop goroutine, so an unbounded wait there stops route
// programming and keeps the SIGTERM handler from ever draining the gateways.
const vtyshCommandTimeout = 10 * time.Second

// commandWaitDelay bounds how long Wait may keep blocking after a command's
// context has expired and the command's process group has been killed.
//
// Wait first drains the output pipe, which reaches EOF only once every process
// holding the write end has exited. killProcessGroup below reaps the whole tree
// on this host, but a wrapper that runs the real command elsewhere — the
// documented `docker exec openvswitch_vswitchd` starts it in another container —
// leaves it holding the pipe. Without a WaitDelay the timeouts above would bound
// nothing at all.
const commandWaitDelay = 5 * time.Second

// killProcessGroup makes a command's timeout reach the processes it forked.
//
// exec.CommandContext's default cancel is Process.Kill, which signals the direct
// child only. Both runners fork through something that in turn forks the process
// that blocks: vtysh spawns per-daemon children, and ovs_wrapper is free-form —
// `nsenter -t 1 -n`, `ip netns exec`, `sudo` and `chroot` all leave the hung
// grandchild running. That grandchild is precisely the command the timeout
// exists to escape, and a reconcile that leaves one behind every cycle
// accumulates them without bound. Giving the child its own process group and
// signalling the group reaps the whole tree.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The whole group is gone: the command finished as the deadline
			// fired. os/exec answers that race by reporting the command's own
			// result, which the default Process.Kill cancel would also do.
			return os.ErrProcessDone
		}
		return err
	}
}

// runVtysh executes a vtysh command under vtyshCommandTimeout, dispatching
// through execVtyshHook when set.
//
// Only stdout is returned. vtysh writes its own diagnostics — a daemon it could
// not reach, a vty socket that went away — to stderr, and every caller below
// parses the result as structured data: running-config stanzas keyed on
// indentation, and JSON. A merged stream would let one stderr line truncate a
// stanza or make a JSON document unrecognisable, which the callers cannot tell
// apart from FRR reporting nothing. On failure stderr goes into the error, where
// it is a diagnostic rather than input.
func (rm *RouteManager) runVtysh(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), vtyshCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "vtysh", args...)
	cmd.WaitDelay = commandWaitDelay
	killProcessGroup(cmd)
	if rm.execVtyshHook != nil {
		return rm.execVtyshHook(cmd)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
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

// validateIP checks that the given string is a valid IPv4 address.
func validateIP(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("invalid IP address: %q", ip)
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

// AddFRRRoutes adds static /32 routes for all given IPs via vtysh.
// IPs are validated before any commands are executed.  The list is chunked
// into batches of frrBatchSize to stay within OS argument-list limits.
func (rm *RouteManager) AddFRRRoutes(ips []string) error {
	if len(ips) == 0 {
		return nil
	}
	for _, ip := range ips {
		if err := validateIP(ip); err != nil {
			return err
		}
	}
	if rm.dryRun {
		slog.Info("[dry-run] would add FRR routes", "count", len(ips), "vrf", rm.vrfName, "nexthop", rm.vethNexthop)
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
			args = append(args, "-c", fmt.Sprintf("ip route %s/32 %s", ip, rm.vethNexthop))
		}
		args = append(args, "-c", "exit-vrf", "-c", "end")
		output, err := rm.runVtysh(args...)
		if err != nil {
			return fmt.Errorf("vtysh batch add %d routes: %w (output: %s)", len(chunk), err, strings.TrimSpace(string(output)))
		}
		slog.Info("FRR routes ensured", "count", len(chunk), "vrf", rm.vrfName, "nexthop", rm.vethNexthop)
	}
	return nil
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

// ListFRRRoutes returns all static /32 routes in the VRF.
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
		// Lines like: S>* 198.51.100.10/32 [1/0] via 169.254.0.1, ...
		if strings.HasPrefix(line, "S") && strings.Contains(line, "/32") {
			parts := strings.Fields(line)
			for _, p := range parts {
				if strings.Contains(p, "/32") {
					ip, _, _ := net.ParseCIDR(p)
					if ip != nil {
						ips = append(ips, ip.String())
					}
					break
				}
			}
		}
	}
	return ips, nil
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
