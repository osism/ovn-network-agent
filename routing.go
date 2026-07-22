package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"sort"
	"strings"
	"syscall"
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
	// cfg is the agent configuration. RouteManager reads the settings it
	// needs straight from it rather than copying them into per-field
	// duplicates: a new config option becomes usable here the moment it is
	// added to Config, with no second place to keep in sync.
	cfg Config

	// ovsWrapper is derived from cfg.OVSWrapper: prefix args for
	// ovs-vsctl/ovs-ofctl (e.g. ["docker", "exec", "openvswitch_vswitchd"]).
	ovsWrapper []string

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

	// refreshVethNexthopHook, when non-nil, replaces RefreshVethNexthop in the
	// next-hop repair. Tests set this to observe that the repair ran, and with
	// which networks, without touching netlink.
	refreshVethNexthopHook func(networks []*net.IPNet) error

	// frrRouteCache memoizes FRR's static-route document for the current
	// reconcile cycle. ListFRRRoutes and InactiveFRRRoutes now issue the
	// identical `show ip route ... static json` query, so without this a
	// no-change cycle would fork vtysh twice for the same document. It is safe
	// as a plain field because the reconcile loop is single-goroutine (see
	// Agent.Run): reconcile, its FRR readers, and shutdown cleanup never run
	// concurrently. AddFRRRoutes/DelFRRRoutes clear it after mutating FRR, and
	// resetFRRRouteCache clears it at the start of each reconcile and cleanup
	// pass, so a reader after a mutation — or in a new pass — always re-reads.
	frrRouteCache map[string][]frrRouteEntry
}

// resetFRRRouteCache drops the memoized FRR static-route document so the next
// reader re-issues the vtysh query. Called at the start of each reconcile and
// cleanup pass; within a pass the memo is shared and invalidated only by a
// route mutation.
func (rm *RouteManager) resetFRRRouteCache() {
	rm.frrRouteCache = nil
}

// runVtysh executes a vtysh command, dispatching through execVtyshHook when set.
func (rm *RouteManager) runVtysh(args ...string) ([]byte, error) {
	cmd := exec.Command("vtysh", args...)
	if rm.execVtyshHook != nil {
		return rm.execVtyshHook(cmd)
	}
	return cmd.CombinedOutput()
}

// warnIfVtyshMissing logs a warning when vtysh cannot be resolved on PATH. The
// agent announces Floating IP /32 routes to BGP through vtysh, but FRR is a
// soft dependency (the unit Wants= it and the package Recommends it), so a host
// installed with --no-install-recommends — or one that has lost FRR — keeps the
// service "active" while every route announcement silently no-ops and is only
// retried on the next reconcile. Emitting this at startup distinguishes a
// missing-FRR host from one where FRR runs out-of-band by design, instead of
// leaving the (unread) route-write-and-retry log as the only signal. lookPath
// is exec.LookPath in production and is injected in tests.
func warnIfVtyshMissing(lookPath func(string) (string, error)) {
	if _, err := lookPath("vtysh"); err != nil {
		slog.Warn("vtysh not found on PATH: BGP route announcements will be logged and retried but never applied until FRR is reachable; install FRR (the package Recommends it) or ensure vtysh is on PATH if FRR runs out-of-band", "error", err)
	}
}

func NewRouteManager(cfg Config) *RouteManager {
	rm := &RouteManager{cfg: cfg}
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

// refreshVethNexthop dispatches to the platform RefreshVethNexthop, or to the
// test hook when one is set.
func (rm *RouteManager) refreshVethNexthop(networks []*net.IPNet) error {
	if rm.refreshVethNexthopHook != nil {
		return rm.refreshVethNexthopHook(networks)
	}
	return rm.RefreshVethNexthop(networks)
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
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would add FRR routes", "count", len(valid), "vrf", rm.cfg.VRFName, "nexthop", rm.cfg.VethNexthop)
		return nil
	}
	// About to mutate FRR's static routes: drop the memo so a subsequent
	// reader in this cycle (verifyRoutes, checkFRRRouteActivity) re-reads.
	rm.frrRouteCache = nil
	var errs []error
	for start := 0; start < len(valid); start += frrBatchSize {
		end := start + frrBatchSize
		if end > len(valid) {
			end = len(valid)
		}
		chunk := valid[start:end]
		args := []string{"-c", "conf t", "-c", fmt.Sprintf("vrf %s", rm.cfg.VRFName)}
		for _, ip := range chunk {
			args = append(args, "-c", fmt.Sprintf("ip route %s/32 %s", ip, rm.cfg.VethNexthop))
		}
		args = append(args, "-c", "exit-vrf", "-c", "end")
		output, err := rm.runVtysh(args...)
		if err != nil {
			slog.Error("failed to add FRR route chunk, continuing with the next",
				"count", len(chunk), "vrf", rm.cfg.VRFName, "error", err, "output", strings.TrimSpace(string(output)))
			errs = append(errs, fmt.Errorf("vtysh batch add %d routes: %w (output: %s)", len(chunk), err, strings.TrimSpace(string(output))))
			continue
		}
		slog.Info("FRR routes ensured", "count", len(chunk), "vrf", rm.cfg.VRFName, "nexthop", rm.cfg.VethNexthop)
	}
	return errors.Join(errs...)
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
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would remove FRR routes", "count", len(ips), "vrf", rm.cfg.VRFName, "nexthop", rm.cfg.VethNexthop)
		return nil
	}
	// About to mutate FRR's static routes: drop the memo so a subsequent
	// reader in this cycle re-reads the post-mutation document.
	rm.frrRouteCache = nil
	for start := 0; start < len(ips); start += frrBatchSize {
		end := start + frrBatchSize
		if end > len(ips) {
			end = len(ips)
		}
		chunk := ips[start:end]
		args := []string{"-c", "conf t", "-c", fmt.Sprintf("vrf %s", rm.cfg.VRFName)}
		for _, ip := range chunk {
			args = append(args, "-c", fmt.Sprintf("no ip route %s/32 %s", ip, rm.cfg.VethNexthop))
		}
		args = append(args, "-c", "exit-vrf", "-c", "end")
		output, err := rm.runVtysh(args...)
		if err != nil {
			return fmt.Errorf("vtysh batch del %d routes: %w (output: %s)", len(chunk), err, strings.TrimSpace(string(output)))
		}
		slog.Info("FRR routes removed", "count", len(chunk), "vrf", rm.cfg.VRFName)
	}
	return nil
}

// frrRouteEntry is the subset of an FRR `show ip route ... json` route object
// that the agent inspects. A static route is only advertised via BGP once it
// is both selected (FRR's best route for the prefix) and installed (present in
// the kernel FIB); FRR omits these keys when false, so the zero value is the
// correct default. Nexthops carries the resolved next-hops, which is what scopes
// a route to this agent (see ListFRRRoutes).
type frrRouteEntry struct {
	Selected  bool `json:"selected"`
	Installed bool `json:"installed"`
	Nexthops  []struct {
		IP string `json:"ip"`
	} `json:"nexthops"`
}

// staticFRRRoutes returns FRR's static routes for the VRF, keyed by prefix, as
// reported by `show ip route vrf <vrf> static json`. It is the single structured
// read shared by ListFRRRoutes and InactiveFRRRoutes: both need the same
// document, and neither may depend on the human-readable table, whose column
// layout FRR is free to change between releases.
func (rm *RouteManager) staticFRRRoutes() (map[string][]frrRouteEntry, error) {
	if rm.frrRouteCache != nil {
		return rm.frrRouteCache, nil
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip route vrf %s static json", rm.cfg.VRFName))
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
	rm.frrRouteCache = routes
	return routes, nil
}

// ListFRRRoutes returns the agent's own static /32 routes in the VRF: those
// installed via the agent's veth nexthop. This nexthop scoping is the FRR
// analog of the kernel protocol tag — without it, reconciliation would treat
// operator-created statics as agent-owned and withdraw them on standby nodes.
//
// The list feeds stale-route withdrawal, so it is read as JSON: a change to
// FRR's human-readable `show ip route` layout would otherwise silently yield an
// empty list and withdraw every FIP the agent advertises.
func (rm *RouteManager) ListFRRRoutes() ([]string, error) {
	if rm.cfg.DryRun {
		return nil, nil
	}
	routes, err := rm.staticFRRRoutes()
	if err != nil {
		return nil, err
	}

	var ips []string
	for prefix, entries := range routes {
		ip, ipNet, err := net.ParseCIDR(prefix)
		if err != nil || ip.To4() == nil {
			continue
		}
		if ones, bits := ipNet.Mask.Size(); ones != 32 || bits != 32 {
			continue
		}
		// Only report statics installed via the agent's own nexthop.
		if !entriesUseNexthop(entries, rm.cfg.VethNexthop) {
			continue
		}
		ips = append(ips, ip.String())
	}
	sort.Strings(ips)
	return ips, nil
}

// entriesUseNexthop reports whether any of a prefix's route entries resolves via
// the given next-hop address.
func entriesUseNexthop(entries []frrRouteEntry, nexthop string) bool {
	for _, e := range entries {
		for _, nh := range e.Nexthops {
			if nh.IP == nexthop {
				return true
			}
		}
	}
	return false
}

// InactiveFRRRoutes returns, from the given /32 IPs, those whose static route
// exists in the VRF but is not selected and installed by FRR — i.e. configured
// but not actually advertised (typically an unresolvable next-hop). IPs with no
// static route at all are not reported: that case is a plain "missing route"
// handled by ensureRoutes. ListFRRRoutes cannot tell the two apart because it
// matches any configured route, which is why this distinct check exists.
func (rm *RouteManager) InactiveFRRRoutes(ips []string) ([]string, error) {
	if rm.cfg.DryRun || len(ips) == 0 {
		return nil, nil
	}
	routes, err := rm.staticFRRRoutes()
	if err != nil {
		return nil, err
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

// VethNexthopResolvable reports whether zebra holds a connected route in the
// VRF that covers the agent's veth next-hop.
//
// Every FIP route the agent announces is an `ip route <fip>/32 <veth-nexthop>`
// static under the VRF, so the connected prefix of the next-hop's own /30 is
// what makes all of them resolvable. Zebra learns that prefix from an
// RTM_NEWADDR on veth-provider, and the kernel does not re-emit one when an
// interface changes VRF — so a zebra that processes the enslavement after
// recording the address can end up holding the interface without its prefix and
// never re-learn it. In that state no static enters the RIB at all: ListFRRRoutes
// reports every route missing, verifyRoutes re-adds them, and the re-add cannot
// help because the configuration was never what was wrong. The FIPs stay
// unreachable and BGP advertises nothing.
//
// The lookup is deliberately scoped to connected routes. Zebra does not resolve
// a static's next-hop through a default route (`ip nht resolve-via-default` is
// off for statics), so matching against the whole RIB would call the next-hop
// resolvable whenever the VRF carries a default and would mask exactly the
// condition this detects. A route type that cannot resolve the next-hop must not
// count as evidence that it does.
func (rm *RouteManager) VethNexthopResolvable() (bool, error) {
	if rm.cfg.DryRun {
		return true, nil
	}
	nexthopIP := net.ParseIP(rm.cfg.VethNexthop)
	if nexthopIP == nil {
		return false, fmt.Errorf("invalid veth nexthop: %q", rm.cfg.VethNexthop)
	}

	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip route vrf %s connected json", rm.cfg.VRFName))
	if err != nil {
		return false, fmt.Errorf("vtysh list connected routes json: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	// FRR emits an empty body rather than "{}" when the VRF has no connected
	// routes; that VRF resolves no next-hop at all.
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return false, nil
	}
	var routes map[string][]frrRouteEntry
	if err := json.Unmarshal([]byte(trimmed), &routes); err != nil {
		return false, fmt.Errorf("parse vtysh connected route json: %w", err)
	}

	for prefix := range routes {
		_, ipNet, err := net.ParseCIDR(prefix)
		if err != nil {
			continue
		}
		if ipNet.Contains(nexthopIP) {
			return true, nil
		}
	}
	return false, nil
}

// RefreshBGP triggers an outbound BGP soft-refresh so that peers learn about
// route changes immediately instead of waiting for the MRAI timer.
func (rm *RouteManager) RefreshBGP() error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would refresh BGP outbound")
		return nil
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("clear ip bgp vrf %s * soft out", rm.cfg.VRFName))
	if err != nil {
		return fmt.Errorf("BGP soft-refresh: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	slog.Info("BGP outbound soft-refresh triggered", "vrf", rm.cfg.VRFName)
	return nil
}

// =============================================================================
// FRR prefix-list management
// =============================================================================

// prefixListEntry represents a single entry in an FRR ip prefix-list. The
// agent manages two entry shapes: "permit <network> ge 32 le 32" for a
// provider network (Exact false) and "permit <vip>/32" for a port-forward VIP
// (Exact true). The exact form is FRR's canonical spelling for a single /32
// (ge/le qualifiers add nothing at full length, and Cisco-style prefix-list
// semantics require len < ge), and the distinct shape lets reconcile tell a
// VIP entry from a network entry: removal must regenerate the config line the
// entry was created with, which is why the shape is carried here.
type prefixListEntry struct {
	Seq     int
	Network string // e.g. "198.51.100.0/24" or "192.0.2.80/32"
	Exact   bool   // true: "permit <p>"; false: "permit <p> ge 32 le 32"
}

// frrPrefixList is the subset of an FRR `show ip prefix-list <name> json`
// document the agent inspects — the innermost object, holding one list's
// entries. FRR wraps it two levels deep: each answering daemon emits
// {"<DAEMON>":{"<name>":{…}}}, keyed first by its own protocol name (ZEBRA,
// BGP) and then by the prefix-list name. vtysh concatenates one such document
// per daemon, so ListFRRPrefixListEntries reads a stream of them and digs
// through the daemon layer (see there). A list that does not exist yields an
// empty body, the structured replacement for the old "Can't find" message.
type frrPrefixList struct {
	Entries []frrPrefixListEntry `json:"entries"`
}

type frrPrefixListEntry struct {
	SequenceNumber int    `json:"sequenceNumber"`
	Type           string `json:"type"`
	Prefix         string `json:"prefix"`
	MinPrefixLen   int    `json:"minimumPrefixLength"`
	MaxPrefixLen   int    `json:"maximumPrefixLength"`
}

// ListFRRPrefixListEntries returns the current entries of the configured FRR
// prefix-list in the two agent-managed shapes: "permit <network> ge 32 le 32"
// and exact "permit <vip>/32". Returns nil if no prefix-list is configured or
// the list does not exist yet.
//
// Read as JSON rather than by parsing the human-readable table: the entries
// drive add/remove of the announced-networks list, so a change to FRR's text
// layout would silently make every entry look absent and churn the list on
// every reconcile.
//
// Safety: cfg.FRRPrefixList is validated by isValidIdentifier (alphanumeric,
// hyphen, underscore, dot) in config validation. Network strings come from
// net.IPNet.String().
func (rm *RouteManager) ListFRRPrefixListEntries() ([]prefixListEntry, error) {
	if rm.cfg.FRRPrefixList == "" {
		return nil, nil
	}
	output, err := rm.runVtysh("-c", fmt.Sprintf("show ip prefix-list %s json", rm.cfg.FRRPrefixList))
	if err != nil {
		return nil, fmt.Errorf("vtysh show prefix-list %s: %w (output: %s)", rm.cfg.FRRPrefixList, err, strings.TrimSpace(string(output)))
	}

	// An empty body (or "{}") means the list does not exist — nothing to
	// reconcile against, not an error.
	trimmed := strings.TrimSpace(string(output))
	if trimmed == "" {
		return nil, nil
	}

	// vtysh runs the show command against every FRR daemon that answers it
	// (zebra and bgpd both store prefix-lists) and concatenates their JSON
	// replies, so the body is a stream of documents — one per daemon — not a
	// single object. A plain json.Unmarshal rejects the second document with
	// "invalid character '{' after top-level value". Each document nests the
	// list under the daemon's own name: {"ZEBRA":{"<name>":{…}}}. Decode the
	// documents in sequence and collect our list's entries from whichever
	// daemons report it, deduplicating by sequence number since the daemons
	// carry the same shared configuration.
	seen := make(map[int]bool)
	var entries []prefixListEntry
	dec := json.NewDecoder(strings.NewReader(trimmed))
	for {
		var doc map[string]map[string]frrPrefixList
		if err := dec.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parse vtysh prefix-list json: %w", err)
		}
		for _, byName := range doc { // keyed by daemon protocol name
			list, ok := byName[rm.cfg.FRRPrefixList]
			if !ok {
				continue
			}
			for _, e := range list.Entries {
				// Only the agent-managed shapes: "permit <network> ge 32
				// le 32" (min and max both present as 32) and the exact
				// "permit <vip>/32", whose JSON carries no
				// minimumPrefixLength/maximumPrefixLength keys at all
				// (verified against the shipped FRR), so both decode to 0.
				if e.Type != "permit" {
					continue
				}
				exact := e.MinPrefixLen == 0 && e.MaxPrefixLen == 0
				if !exact && (e.MinPrefixLen != 32 || e.MaxPrefixLen != 32) {
					continue
				}
				if exact && !strings.HasSuffix(e.Prefix, "/32") {
					continue
				}
				if seen[e.SequenceNumber] {
					continue
				}
				seen[e.SequenceNumber] = true
				entries = append(entries, prefixListEntry{Seq: e.SequenceNumber, Network: e.Prefix, Exact: exact})
			}
		}
	}
	return entries, nil
}

// prefixListLine renders the config line body shared by add and remove:
// "ip prefix-list <name> seq <n> permit <p>[ ge 32 le 32]".
func (rm *RouteManager) prefixListLine(seq int, network string, exact bool) string {
	line := fmt.Sprintf("ip prefix-list %s seq %d permit %s", rm.cfg.FRRPrefixList, seq, network)
	if !exact {
		line += " ge 32 le 32"
	}
	return line
}

// ReconcileFRRPrefixList ensures the managed prefix-list contains exactly one
// "permit <network> ge 32 le 32" entry per desired network and one exact
// "permit <vip>/32" entry per announceable port-forward VIP. The VIP entries
// exist because a VIP need not lie inside any hosted network: the connected
// route it announces through is filtered by this list, and relying on a
// network entry to cover it silently blackholes the VIP as soon as the flat
// and VLAN planes land on different chassis (#226).
// Pass nil, nil to remove all managed entries (cleanup).
func (rm *RouteManager) ReconcileFRRPrefixList(networks []*net.IPNet, vips []string) error {
	if rm.cfg.FRRPrefixList == "" {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would reconcile FRR prefix-list", "name", rm.cfg.FRRPrefixList, "networks", len(networks), "vips", len(vips))
		return nil
	}

	current, err := rm.ListFRRPrefixListEntries()
	if err != nil {
		return fmt.Errorf("list prefix-list entries: %w", err)
	}

	// Key current and desired by prefix + shape: the same prefix string in the
	// other shape is a different config line and must not satisfy the check.
	type entryKey struct {
		network string
		exact   bool
	}
	currentByKey := make(map[entryKey]int, len(current)) // → seq
	maxSeq := 0
	for _, e := range current {
		currentByKey[entryKey{e.Network, e.Exact}] = e.Seq
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}

	desired := make(map[entryKey]bool, len(networks)+len(vips))
	for _, n := range networks {
		desired[entryKey{n.String(), false}] = true
	}
	for _, vip := range vips {
		desired[entryKey{vip + "/32", true}] = true
	}

	// Add missing entries (before removing stale ones, to avoid a window with no entries).
	for key := range desired {
		if _, exists := currentByKey[key]; !exists {
			maxSeq += 5
			output, err := rm.runVtysh(
				"-c", "conf t",
				"-c", rm.prefixListLine(maxSeq, key.network, key.exact),
				"-c", "end",
			)
			if err != nil {
				return fmt.Errorf("add prefix-list entry %s seq %d: %w (output: %s)", key.network, maxSeq, err, strings.TrimSpace(string(output)))
			}
			slog.Info("FRR prefix-list entry added", "name", rm.cfg.FRRPrefixList, "network", key.network, "seq", maxSeq)
		}
	}

	// Remove stale entries.
	for key, seq := range currentByKey {
		if !desired[key] {
			output, err := rm.runVtysh(
				"-c", "conf t",
				"-c", "no "+rm.prefixListLine(seq, key.network, key.exact),
				"-c", "end",
			)
			if err != nil {
				return fmt.Errorf("remove prefix-list entry %s seq %d: %w (output: %s)", key.network, seq, err, strings.TrimSpace(string(output)))
			}
			slog.Info("FRR prefix-list entry removed", "name", rm.cfg.FRRPrefixList, "network", key.network, "seq", seq)
		}
	}

	return nil
}

// =============================================================================
// Helpers
// =============================================================================

// isNoSuchRoute and isNoSuchRule report the "it was already gone" outcome of a
// netlink delete, which is success for an idempotent teardown.
//
// The kernel answers a missing route with ESRCH and a missing rule with ENOENT,
// and netlink surfaces them as syscall.Errno values. Match on the errno with
// errors.Is rather than on the rendered message: the text is produced by the
// C library's strerror table and is locale- and version-dependent, so a
// substring check silently turns "already gone" into a hard error on any host
// that words it differently. isFileExists (routing_linux.go) already does it
// this way.
func isNoSuchRoute(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}

func isNoSuchRule(err error) bool {
	return errors.Is(err, syscall.ENOENT)
}

// isNoSuchAddr reports the same "it was already gone" outcome for an address
// delete, which the kernel answers with EADDRNOTAVAIL. RefreshVethNexthop
// deletes an address only to re-add it, so an address that is already absent
// leaves it with exactly the work it meant to do.
func isNoSuchAddr(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL)
}
