package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/vishvananda/netlink"
)

// nftCmd runs an nft command with the given arguments.
func nftCmd(args ...string) ([]byte, error) {
	cmd := exec.Command("nft", args...)
	return cmd.CombinedOutput()
}

// nftTableExists reports whether the agent's nft table is currently present,
// asking nft for its JSON table listing rather than inferring existence from
// the wording of a failed delete. See nftHasTable.
func nftTableExists() (bool, error) {
	out, err := nftCmd("-j", "list", "tables")
	if err != nil {
		return false, fmt.Errorf("nft list tables: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nftHasTable(out, "ip", nftTableName)
}

// SetupPortForward performs the initial port forwarding setup.
// Must be called after SetupVethLeak (requires the veth pair to exist).
func (rm *RouteManager) SetupPortForward() error {
	if !rm.cfg.PortForwardEnabled {
		slog.Debug("port forwarding disabled, skipping setup")
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would set up port forwarding",
			"dev", rm.cfg.PortForwardDev,
			"table", rm.cfg.PortForwardTableID,
			"vips", len(rm.cfg.PortForwards),
		)
		return nil
	}

	// Verify nft binary is available.
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("nft binary not found in PATH (required for port forwarding): %w", err)
	}

	// 1. Manage VIP addresses on loopback device.
	if err := rm.reconcilePortForwardVIPs(); err != nil {
		return fmt.Errorf("manage VIP addresses: %w", err)
	}

	// 2. Apply nftables ruleset (initial, without provider networks or SNAT IPs).
	if err := rm.applyNftRuleset(nil, nil); err != nil {
		return fmt.Errorf("apply nftables ruleset: %w", err)
	}

	// 3. Ensure IP forwarding is enabled on the veth interfaces.
	if err := rm.ensureVethForwarding(); err != nil {
		return fmt.Errorf("enable forwarding on veth: %w", err)
	}

	// 4. Policy routing: steer DNAT'd traffic via main table (forward) and
	// VRF table (reply return path).
	if err := rm.ensureDNATRouting(); err != nil {
		return fmt.Errorf("DNAT policy routing: %w", err)
	}

	slog.Info("port forwarding setup complete",
		"dev", rm.cfg.PortForwardDev,
		"table", rm.cfg.PortForwardTableID,
		"vips", len(rm.cfg.PortForwards),
	)
	return nil
}

// ReconcilePortForward ensures port forwarding state matches the desired config.
// providerNetworks are needed for the forward_veth_guard chain (allow existing
// veth leak return traffic in addition to DNAT return traffic).
// snatIPs are the router SNAT external IPs from OVN state, used for
// router_masquerade rules.
func (rm *RouteManager) ReconcilePortForward(providerNetworks []*net.IPNet, snatIPs []string) error {
	if !rm.cfg.PortForwardEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would reconcile port forwarding", "vips", len(rm.cfg.PortForwards))
		return nil
	}

	if err := rm.reconcilePortForwardVIPs(); err != nil {
		return fmt.Errorf("reconcile VIP addresses: %w", err)
	}
	if err := rm.applyNftRuleset(providerNetworks, snatIPs); err != nil {
		return fmt.Errorf("reconcile nftables ruleset: %w", err)
	}
	if err := rm.ensureDNATRouting(); err != nil {
		return fmt.Errorf("reconcile DNAT routing: %w", err)
	}
	return nil
}

// TeardownPortForward removes all port forwarding resources.
// Must be called before TeardownVethLeak. Best-effort: attempts all
// cleanup steps and returns the first error encountered.
func (rm *RouteManager) TeardownPortForward() error {
	if !rm.cfg.PortForwardEnabled {
		slog.Debug("port forwarding disabled, skipping teardown")
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would tear down port forwarding")
		return nil
	}

	var firstErr error
	recordErr := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	// 1. Delete nftables table (stops all DNAT immediately). Ask nft whether
	// the table exists first, rather than deleting blindly and treating an
	// error whose text contains "No such file" as benign: with the existence
	// known up front, any delete failure is a real one.
	switch exists, err := nftTableExists(); {
	case err != nil:
		slog.Warn("failed to list nftables tables", "error", err)
		recordErr(fmt.Errorf("list nftables tables: %w", err))
	case !exists:
		slog.Debug("nftables table already absent, nothing to delete", "table", nftTableName)
	default:
		if out, err := nftCmd("delete", "table", "ip", nftTableName); err != nil {
			slog.Warn("failed to delete nftables table", "error", err, "output", strings.TrimSpace(string(out)))
			recordErr(fmt.Errorf("delete nftables table: %w", err))
		} else {
			slog.Info("nftables table removed", "table", nftTableName)
		}
	}

	// 2. Remove DNAT policy routing (ip rule + routes in table).
	rm.cleanupDNATRouting()

	// 3. Remove managed VIP addresses.
	link, err := netlink.LinkByName(rm.cfg.PortForwardDev)
	if err == nil {
		for _, pf := range rm.cfg.PortForwards {
			if !pf.ManageVIP {
				continue
			}
			vipIP := net.ParseIP(pf.VIP)
			addr := &netlink.Addr{
				IPNet: &net.IPNet{IP: vipIP, Mask: net.CIDRMask(32, 32)},
			}
			if err := netlink.AddrDel(link, addr); err != nil {
				slog.Warn("failed to remove VIP address", "vip", pf.VIP, "error", err)
				recordErr(fmt.Errorf("remove VIP %s: %w", pf.VIP, err))
			} else {
				slog.Info("VIP address removed", "vip", pf.VIP, "dev", rm.cfg.PortForwardDev)
			}
		}
	}

	slog.Info("port forwarding teardown complete")
	return firstErr
}

// rtTableMain is the Linux main routing table ID (RT_TABLE_MAIN = 254).
const rtTableMain = 254

// ensureDNATRouting adds two ip rules and one route for bidirectional DNAT
// policy routing:
//
//  1. Forward (original direction): fwmark 0x100 → lookup main (priority 150)
//     Escapes the VRF so DNAT'd traffic reaches the backend via the default
//     VRF's routing (e.g. control-plane network).
//
//  2. Reply (return direction): fwmark 0x200 → lookup portForwardTableID (priority 151)
//     Routes reply traffic through the veth pair back into the provider VRF.
//     This avoids cross-VRF forwarding which triggers rp_filter false-positives.
//     Table portForwardTableID contains: default via <vethProviderIP> dev veth-default.
//
// All operations are idempotent.
func (rm *RouteManager) ensureDNATRouting() error {
	// Forward rule: fwmark 0x100 → lookup main.
	fwdMask := uint32(dnatFwmark)
	fwdRule := netlink.NewRule()
	fwdRule.Mark = dnatFwmark
	fwdRule.Mask = &fwdMask
	fwdRule.Table = rtTableMain
	fwdRule.Priority = dnatFwmarkPriority
	if err := netlink.RuleAdd(fwdRule); err != nil {
		if !isFileExists(err) {
			return fmt.Errorf("add ip rule fwmark 0x%x lookup main: %w", dnatFwmark, err)
		}
	}

	// Reply rule: fwmark 0x200 → lookup portForwardTableID (via veth pair).
	replyMask := uint32(dnatReplyFwmark)
	replyRule := netlink.NewRule()
	replyRule.Mark = dnatReplyFwmark
	replyRule.Mask = &replyMask
	replyRule.Table = rm.cfg.PortForwardTableID
	replyRule.Priority = dnatReplyPriority
	if err := netlink.RuleAdd(replyRule); err != nil {
		if !isFileExists(err) {
			return fmt.Errorf("add ip rule fwmark 0x%x lookup %d: %w", dnatReplyFwmark, rm.cfg.PortForwardTableID, err)
		}
	}

	// Default route in portForwardTableID via veth pair. Reply traffic goes
	// through veth-default → veth-provider, entering the provider VRF properly.
	// This avoids cross-VRF forwarding that triggers rp_filter drops.
	vethLink, err := netlink.LinkByName(vethDefaultName)
	if err != nil {
		return fmt.Errorf("find %s for DNAT reply route: %w", vethDefaultName, err)
	}
	providerIP := net.ParseIP(rm.cfg.VethProviderIP)
	if err := netlink.RouteReplace(&netlink.Route{
		Dst:       &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
		LinkIndex: vethLink.Attrs().Index,
		Gw:        providerIP,
		Table:     rm.cfg.PortForwardTableID,
	}); err != nil {
		return fmt.Errorf("add default route via %s table %d: %w", vethDefaultName, rm.cfg.PortForwardTableID, err)
	}

	slog.Debug("DNAT policy routing ensured",
		"fwd_table", "main",
		"reply_table", rm.cfg.PortForwardTableID,
		"reply_via", vethDefaultName,
	)
	return nil
}

// cleanupDNATRouting removes both DNAT ip rules and the reply route.
// Best-effort: logs warnings on failure.
func (rm *RouteManager) cleanupDNATRouting() {
	// Remove forward rule.
	fwdMask := uint32(dnatFwmark)
	fwdRule := netlink.NewRule()
	fwdRule.Mark = dnatFwmark
	fwdRule.Mask = &fwdMask
	fwdRule.Table = rtTableMain
	fwdRule.Priority = dnatFwmarkPriority
	if err := netlink.RuleDel(fwdRule); err != nil {
		slog.Debug("DNAT forward ip rule already absent", "error", err)
	} else {
		slog.Info("DNAT forward ip rule removed")
	}

	// Remove reply rule.
	replyMask := uint32(dnatReplyFwmark)
	replyRule := netlink.NewRule()
	replyRule.Mark = dnatReplyFwmark
	replyRule.Mask = &replyMask
	replyRule.Table = rm.cfg.PortForwardTableID
	replyRule.Priority = dnatReplyPriority
	if err := netlink.RuleDel(replyRule); err != nil {
		slog.Debug("DNAT reply ip rule already absent", "error", err)
	} else {
		slog.Info("DNAT reply ip rule removed")
	}

	// Flush routes from the port-forward reply table.
	filter := &netlink.Route{Table: rm.cfg.PortForwardTableID}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_TABLE)
	if err == nil {
		for _, r := range routes {
			_ = netlink.RouteDel(&r)
		}
	}
}

// reconcilePortForwardVIPs ensures managed VIP /32 addresses are present on the
// loopback device. Uses AddrReplace for idempotency.
func (rm *RouteManager) reconcilePortForwardVIPs() error {
	link, err := netlink.LinkByName(rm.cfg.PortForwardDev)
	if err != nil {
		return fmt.Errorf("find device %s: %w", rm.cfg.PortForwardDev, err)
	}

	for _, pf := range rm.cfg.PortForwards {
		if !pf.ManageVIP {
			continue
		}
		vipIP := net.ParseIP(pf.VIP)
		addr := &netlink.Addr{
			IPNet: &net.IPNet{IP: vipIP, Mask: net.CIDRMask(32, 32)},
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			return fmt.Errorf("add VIP %s/32 to %s: %w", pf.VIP, rm.cfg.PortForwardDev, err)
		}
		slog.Debug("VIP address ensured", "vip", pf.VIP, "dev", rm.cfg.PortForwardDev)
	}
	return nil
}

// applyNftRuleset atomically replaces the nftables table with the current config.
// The delete + create is submitted as a single nft -f input to avoid a window
// where no rules exist.
func (rm *RouteManager) applyNftRuleset(providerNetworks []*net.IPNet, snatIPs []string) error {
	ruleset := buildNftRuleset(rm.cfg.PortForwards, providerNetworks, snatIPs, rm.cfg.PortForwardCTZone)

	// Combine delete (if exists) and create into a single atomic nft load.
	// The delete may fail on first run (table doesn't exist yet), so we
	// use "delete table" only when we know it exists.
	var atomic strings.Builder
	// Check if table exists by listing it.
	if _, err := nftCmd("list", "table", "ip", nftTableName); err == nil {
		fmt.Fprintf(&atomic, "delete table ip %s\n", nftTableName)
	}
	atomic.WriteString(ruleset)

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(atomic.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft apply ruleset: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	slog.Debug("nftables ruleset applied", "table", nftTableName)
	return nil
}

// ensureVethForwarding enables IP forwarding on the veth interfaces and, when
// port forwarding is active, sets accept_local=1 on veth-provider. The latter
// is needed because DNAT reply traffic re-enters the VRF via veth-provider with
// a source address (the VIP) that is local within the VRF. Without accept_local
// the kernel drops these packets as martians.
//
// When port_forward_l3mdev_accept is true, the global udp/tcp_l3mdev_accept
// sysctls are set to 1. This is needed when a DNAT backend runs on the same
// host but in a different VRF than the VIP (e.g. backend on loopback0 in the
// default VRF, VIP on loopback1 in the provider VRF). Without l3mdev_accept
// the kernel refuses to deliver the DNAT'd packet to the backend socket across
// VRF boundaries.
//
// NOTE: l3mdev_accept is a global sysctl that allows ALL sockets to receive
// packets from any VRF. Only enable when same-host cross-VRF backends are used.
func (rm *RouteManager) ensureVethForwarding() error {
	for _, dev := range []string{vethDefaultName, vethProviderName} {
		path := filepath.Join("/proc/sys/net/ipv4/conf", dev, "forwarding")
		if err := os.WriteFile(path, []byte("1\n"), 0644); err != nil {
			return fmt.Errorf("enable forwarding on %s: %w", dev, err)
		}
	}
	if rm.cfg.PortForwardEnabled {
		path := filepath.Join("/proc/sys/net/ipv4/conf", vethProviderName, "accept_local")
		if err := os.WriteFile(path, []byte("1\n"), 0644); err != nil {
			return fmt.Errorf("enable accept_local on %s: %w", vethProviderName, err)
		}
		if rm.cfg.PortForwardL3mdevAccept {
			for _, sysctl := range []string{
				"/proc/sys/net/ipv4/udp_l3mdev_accept",
				"/proc/sys/net/ipv4/tcp_l3mdev_accept",
			} {
				if err := os.WriteFile(sysctl, []byte("1\n"), 0644); err != nil {
					return fmt.Errorf("enable %s: %w", filepath.Base(sysctl), err)
				}
			}
			slog.Info("l3mdev_accept enabled for cross-VRF same-host DNAT backends")
		}
	}
	slog.Debug("IP forwarding enabled on veth interfaces")
	return nil
}
