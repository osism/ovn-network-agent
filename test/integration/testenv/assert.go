//go:build integration

package testenv

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// AssertKernelRoute fails the test if no /32 route for ip exists on the
// default bridge device. Polls for up to timeout to give the agent time
// to install the route after a state change.
func AssertKernelRoute(t *testing.T, ip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(ip) == nil {
		t.Fatalf("AssertKernelRoute: invalid IP %q", ip)
	}
	deadline := time.Now().Add(timeout)
	var lastOut string
	var lastErr error
	for {
		out, err := exec.Command("ip", "-4", "route", "show", ip+"/32", "dev", DefaultBridgeDev).CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		lastErr = err
		if err == nil && strings.Contains(string(out), ip) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("kernel route %s/32 on %s not present after %s (last output: %q, err: %v)",
				ip, DefaultBridgeDev, timeout, lastOut, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoKernelRoute fails the test if a /32 route for ip persists on the
// bridge device past timeout. Mirrors AssertKernelRoute for the negative case.
func AssertNoKernelRoute(t *testing.T, ip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(ip) == nil {
		t.Fatalf("AssertNoKernelRoute: invalid IP %q", ip)
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ip", "-4", "route", "show", ip+"/32", "dev", DefaultBridgeDev).CombinedOutput()
		if err == nil && !strings.Contains(string(out), ip) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("kernel route %s/32 on %s still present after %s (last output: %q)",
				ip, DefaultBridgeDev, timeout, strings.TrimSpace(string(out)))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertKernelRouteOnDev fails the test if no /32 route for ip exists on the
// named interface within timeout. The multi-segment scenarios use it to pin
// each FIP's route to its segment's VLAN subinterface rather than just "some
// interface on the host".
func AssertKernelRouteOnDev(t *testing.T, ip, dev string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(ip) == nil {
		t.Fatalf("AssertKernelRouteOnDev: invalid IP %q", ip)
	}
	deadline := time.Now().Add(timeout)
	var lastOut string
	var lastErr error
	for {
		out, err := exec.Command("ip", "-4", "route", "show", ip+"/32", "dev", dev).CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		lastErr = err
		if err == nil && strings.Contains(string(out), ip) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("kernel route %s/32 on %s not present after %s (last output: %q, err: %v)",
				ip, dev, timeout, lastOut, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertLinkExists fails the test if the named link does not appear within
// timeout. Used for the agent-created per-VLAN segment interfaces.
func AssertLinkExists(t *testing.T, dev string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if _, err := net.InterfaceByName(dev); err == nil {
			return
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("link %s not present after %s (last error: %v)", dev, timeout, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoLink fails the test if the named link persists past timeout.
// Mirrors AssertLinkExists for the negative case (segment pruning, teardown).
func AssertNoLink(t *testing.T, dev string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := net.InterfaceByName(dev); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("link %s still present after %s", dev, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertBridgeAddress fails the test if cidr is not present on bridge within
// timeout. cidr is matched against `ip -j -4 addr show dev <bridge>` as the
// (local, prefixlen) pair extracted from each addr_info entry, so the caller
// must pass the address in CIDR form (e.g. "169.254.169.254/32") to be
// unambiguous about the mask length.
//
// Used by the bridge-IP lifecycle scenarios (#63): on cold start the agent
// adds BridgeIP to br-ex via EnsureBridgeIP, and several scenarios assert
// that exactly one such address exists, that a custom address replaces the
// default, and that cleanup removes it.
func AssertBridgeAddress(t *testing.T, bridge, cidr string, timeout time.Duration) {
	t.Helper()
	ip, prefix, err := parseCIDRForBridge(cidr)
	if err != nil {
		t.Fatalf("AssertBridgeAddress: %v", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		count, lastOut, lastErr := countBridgeAddress(bridge, ip, prefix)
		if count == 1 {
			return
		}
		if count > 1 {
			t.Fatalf("bridge %s has %d copies of %s (expected exactly 1); last output: %q",
				bridge, count, cidr, lastOut)
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge address %s on %s not present after %s (last output: %q, err: %v)",
				cidr, bridge, timeout, lastOut, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoBridgeAddress fails the test if cidr remains on bridge past timeout.
// Mirrors AssertBridgeAddress for the negative case — used to verify that
// CleanupOnShutdown removes the BridgeIP and that a configured non-default
// BridgeIP excludes the default.
func AssertNoBridgeAddress(t *testing.T, bridge, cidr string, timeout time.Duration) {
	t.Helper()
	ip, prefix, err := parseCIDRForBridge(cidr)
	if err != nil {
		t.Fatalf("AssertNoBridgeAddress: %v", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		count, lastOut, _ := countBridgeAddress(bridge, ip, prefix)
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("bridge address %s still present on %s after %s (last output: %q)",
				cidr, bridge, timeout, lastOut)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// parseCIDRForBridge splits "<ip>/<prefix>" into the canonical IP form and
// integer prefix length. Rejects bare IPs so callers cannot accidentally
// match a /24 entry when they meant /32, and rejects IPv6 because the
// callers only assert against `ip -j -4 addr show`.
func parseCIDRForBridge(cidr string) (string, int, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", 0, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	if ip.To4() == nil {
		return "", 0, fmt.Errorf("invalid CIDR %q: IPv4 required", cidr)
	}
	prefix, _ := ipnet.Mask.Size()
	return ip.String(), prefix, nil
}

// countBridgeAddress runs `ip -j -4 addr show dev <bridge>` and returns the
// number of addr_info entries whose (local, prefixlen) match (ip, prefix).
// Returns the raw stdout for inclusion in diagnostic messages.
func countBridgeAddress(bridge, ip string, prefix int) (int, string, error) {
	out, err := exec.Command("ip", "-j", "-4", "addr", "show", "dev", bridge).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return 0, text, err
	}
	type addrInfo struct {
		Local     string `json:"local"`
		Prefixlen int    `json:"prefixlen"`
	}
	var entries []struct {
		AddrInfo []addrInfo `json:"addr_info"`
	}
	if jerr := json.Unmarshal(out, &entries); jerr != nil {
		return 0, text, jerr
	}
	count := 0
	for _, e := range entries {
		for _, a := range e.AddrInfo {
			if a.Local == ip && a.Prefixlen == prefix {
				count++
			}
		}
	}
	return count, text, nil
}

// AssertOVSFlow fails the test if no flow with the given cookie exists on
// br-ex. Cookie format mirrors ovs-ofctl: e.g. "0x999".
func AssertOVSFlow(t *testing.T, cookie string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ovs-ofctl", "dump-flows", DefaultBridgeDev,
			fmt.Sprintf("cookie=%s/-1", cookie)).CombinedOutput()
		if err == nil {
			lines := 0
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "cookie=") {
					lines++
				}
			}
			if lines > 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("OVS flow with cookie %s on %s not present after %s (last output: %q, err: %v)",
				cookie, DefaultBridgeDev, timeout, strings.TrimSpace(string(out)), err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoOVSFlow fails the test if any flow with the given cookie persists
// on the bridge device past timeout. Used to verify cleanup paths.
func AssertNoOVSFlow(t *testing.T, cookie string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ovs-ofctl", "dump-flows", DefaultBridgeDev,
			fmt.Sprintf("cookie=%s/-1", cookie)).CombinedOutput()
		if err == nil {
			lines := 0
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "cookie=") {
					lines++
				}
			}
			if lines == 0 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("OVS flows with cookie %s on %s still present after %s (last output: %q)",
				cookie, DefaultBridgeDev, timeout, strings.TrimSpace(string(out)))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoOVSFlowMatches polls dump-flows for the cookie and fails if any
// line still satisfies match past timeout. Mirrors AssertOVSFlowMatches for
// the negative case: useful when several flows share a cookie (e.g. v4 and
// v6 hairpin flows under 0x998) and the test wants to verify cleanup of one
// specific entry without disturbing the others.
func AssertNoOVSFlowMatches(t *testing.T, cookie string, match func(line string) bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ovs-ofctl", "dump-flows", DefaultBridgeDev,
			fmt.Sprintf("cookie=%s/-1", cookie)).CombinedOutput()
		if err == nil {
			matched := false
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, "cookie=") {
					continue
				}
				if match(line) {
					matched = true
					break
				}
			}
			if !matched {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("OVS flow with cookie %s matching %s still present after %s (last output: %q)",
				cookie, msg, timeout, strings.TrimSpace(string(out)))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertOVSFlowMatches polls dump-flows for the cookie and runs match against
// each line containing "cookie=". Useful when several flows share a cookie
// (e.g. hairpin flows for several IPs) and the test wants to check a specific
// IP/MAC combination.
func AssertOVSFlowMatches(t *testing.T, cookie string, match func(line string) bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ovs-ofctl", "dump-flows", DefaultBridgeDev,
			fmt.Sprintf("cookie=%s/-1", cookie)).CombinedOutput()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if !strings.Contains(line, "cookie=") {
					continue
				}
				if match(line) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no OVS flow with cookie %s matching %s after %s (last output: %q)",
				cookie, msg, timeout, strings.TrimSpace(string(out)))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertFRRRoute fails the test if no static /32 route for ip exists in
// the default VRF.
func AssertFRRRoute(t *testing.T, ip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(ip) == nil {
		t.Fatalf("AssertFRRRoute: invalid IP %q", ip)
	}
	deadline := time.Now().Add(timeout)
	var routes []string
	var err error
	for {
		routes, err = frrStaticRoutes(DefaultVRFName)
		if err == nil {
			for _, r := range routes {
				if r == ip {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("FRR static route %s/32 in vrf %s not present after %s (current: %v, err: %v)",
				ip, DefaultVRFName, timeout, routes, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertNoFRRRoute fails the test if the static /32 route for ip persists in
// the VRF past timeout.
func AssertNoFRRRoute(t *testing.T, ip string, timeout time.Duration) {
	t.Helper()
	if net.ParseIP(ip) == nil {
		t.Fatalf("AssertNoFRRRoute: invalid IP %q", ip)
	}
	deadline := time.Now().Add(timeout)
	for {
		routes, err := frrStaticRoutes(DefaultVRFName)
		if err == nil {
			present := false
			for _, r := range routes {
				if r == ip {
					present = true
					break
				}
			}
			if !present {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("FRR static route %s/32 in vrf %s still present after %s",
				ip, DefaultVRFName, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertNftRule fails the test if `nft list table ip <DefaultNftTable>`
// does not contain substring. The substring must be specific enough to
// uniquely identify the rule (e.g. a VIP address combined with a port).
func AssertNftRule(t *testing.T, substring string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("nft", "list", "table", "ip", DefaultNftTable).CombinedOutput()
		if err == nil && strings.Contains(string(out), substring) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("nft table %s does not contain %q after %s (err: %v, output: %q)",
				DefaultNftTable, substring, timeout, err, strings.TrimSpace(string(out)))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// scrubLocalState removes everything the agent might have installed on this
// host: nft table, /32 routes on br-ex, FRR static routes, and OVS flows
// matching the agent's cookies. Best-effort — used between tests so a single
// failed test does not poison the next.
func scrubLocalState(t *testing.T) {
	t.Helper()

	if out, err := exec.Command("nft", "list", "table", "ip", DefaultNftTable).CombinedOutput(); err == nil && len(out) > 0 {
		_ = exec.Command("nft", "delete", "table", "ip", DefaultNftTable).Run()
	}

	// Flush all /32 routes on br-ex (regardless of protocol) since the agent
	// adds them with the kernel default protocol when no route_table_id is
	// configured. This is heavier-handed than Teardown but safer between
	// scenario tests.
	out, err := exec.Command("ip", "-4", "route", "show", "dev", DefaultBridgeDev).CombinedOutput()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			dst := fields[0]
			if !strings.HasSuffix(dst, "/32") {
				continue
			}
			_ = exec.Command("ip", "route", "del", dst, "dev", DefaultBridgeDev).Run()
		}
	}

	// FRR static /32 routes in the test VRF.
	if routes, err := frrStaticRoutes(DefaultVRFName); err == nil {
		for _, ip := range routes {
			args := []string{
				"-c", "conf t",
				"-c", "vrf " + DefaultVRFName,
				"-c", "no ip route " + ip + "/32",
				"-c", "exit-vrf",
				"-c", "end",
			}
			_ = exec.Command("vtysh", args...).Run()
		}
	}

	// OVS flows tagged with the agent's cookies.
	for _, cookie := range []string{"0x999", "0x998"} {
		_ = exec.Command("ovs-ofctl", "del-flows", DefaultBridgeDev,
			fmt.Sprintf("cookie=%s/-1", cookie)).Run()
	}

	// Agent-created per-VLAN segment interfaces on the bridge. Deleting a
	// link also drops the /32 routes riding on it, so no per-device route
	// flush is needed here.
	scrubSegmentLinks(t)

	// Port-forward residue: managed VIPs on loopback1, fwmark ip rules,
	// the port-forward reply table. Calls into portforward.go.
	scrubPortForwardState(t)

	// Veth-leak residue: per-network policy rules at the agent's priority,
	// the leak table, and the veth pair itself. SIGKILL'd agents (the test
	// crashed before WithAgent's Stop ran) skip the agent's TeardownVethLeak,
	// so without this scrub the next scenario inherits an enslaved veth pair.
	scrubVethLeakState(t)
}
