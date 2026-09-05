//go:build integration

package testenv

import (
	"encoding/json"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Veth-leak-related constants. They mirror the agent's defaults
// (vethDefaultName/vethProviderName from routing.go and the table/priority
// defaults from config.go) so scenario tests can refer to a single source of
// truth instead of duplicating literals across files.
const (
	// VethDefaultName is the default-VRF side of the veth pair created by
	// SetupVethLeak.
	VethDefaultName = "veth-default"

	// VethProviderName is the provider-VRF side of the veth pair. It is
	// enslaved to vrf-provider on agent startup.
	VethProviderName = "veth-provider"

	// DefaultVethNexthop matches the agent's default veth_nexthop and is
	// also what testenv.Defaults() configures: the address assigned to
	// veth-default and the gateway for per-network VRF routes installed by
	// ReconcileVethLeakNetworks.
	DefaultVethNexthop = "169.254.0.1"

	// DefaultVethProviderIP is the address assigned to veth-provider — the
	// agent computes it as veth_nexthop + 1 when veth_provider_ip is unset.
	DefaultVethProviderIP = "169.254.0.2"

	// DefaultVethLeakTableID matches the agent's default veth_leak_table_id.
	// The default route via veth-default lives in this table.
	DefaultVethLeakTableID = 200

	// DefaultVethLeakRulePriority matches the agent's default
	// veth_leak_rule_priority. Per-network policy rules installed by
	// ReconcileVethLeakNetworks live at this priority.
	DefaultVethLeakRulePriority = 2000

	// VethLeakRouteProtocol mirrors rtProtoOVNNetworkAgent in routing_linux.go
	// — the custom rtproto the agent tags its per-network VRF routes with so
	// they can be distinguished from FRR-installed entries.
	VethLeakRouteProtocol = 44
)

// linkInfo is the subset of `ip -j -d link show` we care about. The
// `linkinfo.info_data.table` path is how the kernel exposes a VRF device's
// associated routing-table id. `master` reports the device a slave is
// enslaved to.
type linkInfo struct {
	IfName    string `json:"ifname"`
	Master    string `json:"master,omitempty"`
	OperState string `json:"operstate,omitempty"`
	Flags     []string
	LinkInfo  struct {
		InfoKind string `json:"info_kind,omitempty"`
		InfoData struct {
			Table int `json:"table,omitempty"`
		} `json:"info_data,omitempty"`
	} `json:"linkinfo,omitempty"`
}

// routeInfo is the subset of `ip -j route show` fields used by veth-leak
// assertions. `Protocol` is rendered as either an int or a string by iproute2
// depending on whether a name is registered for the protocol number; we
// accept both via json.RawMessage and parse defensively.
type routeInfo struct {
	Dst      string          `json:"dst"`
	Gateway  string          `json:"gateway,omitempty"`
	Dev      string          `json:"dev,omitempty"`
	Protocol json.RawMessage `json:"protocol,omitempty"`
}

// ruleInfo is the subset of `ip -j rule show` fields relevant to veth-leak
// per-network rules: priority, source IP + prefix length, and the destination
// table. iproute2 splits the CIDR across two fields (`src` is the bare IP,
// `srclen` is the prefix bits) — srcCIDR reassembles them. When there is no
// source match, iproute2 emits `"src":"all"` and omits srclen.
type ruleInfo struct {
	Priority int             `json:"priority"`
	Src      string          `json:"src,omitempty"`
	Srclen   int             `json:"srclen,omitempty"`
	Dst      string          `json:"dst,omitempty"`
	To       string          `json:"to,omitempty"`
	Dstlen   int             `json:"dstlen,omitempty"`
	DstLen   int             `json:"dst_len,omitempty"`
	Table    json.RawMessage `json:"table,omitempty"`
}

// srcCIDR rebuilds the rule's source CIDR from src + srclen. Returns "" when
// the rule has no source filter (iproute2 reports "all"). Tests can compare
// the result directly to a `net.ParseCIDR`-shaped string.
func (r ruleInfo) srcCIDR() string {
	if r.Src == "" || r.Src == "all" {
		return ""
	}
	return r.Src + "/" + strconv.Itoa(r.Srclen)
}

// dstCIDR rebuilds the rule's destination CIDR from dst + dst_len. iproute2
// uses "all" for an unqualified destination, just as it does for source.
func (r ruleInfo) dstCIDR() string {
	dst := r.Dst
	if dst == "" {
		dst = r.To
	}
	if dst == "" || dst == "all" {
		return ""
	}
	if strings.Contains(dst, "/") {
		return dst
	}
	prefixLen := r.Dstlen
	if prefixLen == 0 {
		prefixLen = r.DstLen
	}
	return dst + "/" + strconv.Itoa(prefixLen)
}

// readLink invokes `ip -j -d link show <name>` and decodes the first entry.
// Returns false if the link does not exist.
func readLink(t *testing.T, name string) (linkInfo, bool) {
	t.Helper()
	out, err := exec.Command("ip", "-j", "-d", "link", "show", name).CombinedOutput()
	if err != nil {
		// "Device not found" exits non-zero; treat that as absent, not an error.
		return linkInfo{}, false
	}
	var arr []linkInfo
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatalf("parse link json for %s: %v (raw: %q)", name, err, string(out))
	}
	if len(arr) == 0 {
		return linkInfo{}, false
	}
	return arr[0], true
}

// VRFTableID looks up the routing table id associated with a VRF device.
// Mirrors the agent's getVRFTableID helper in routing_linux.go.
func VRFTableID(t *testing.T, vrf string) int {
	t.Helper()
	li, ok := readLink(t, vrf)
	if !ok {
		t.Fatalf("VRFTableID: VRF device %s not found", vrf)
	}
	if li.LinkInfo.InfoKind != "vrf" {
		t.Fatalf("VRFTableID: %s is not a VRF (kind=%q)", vrf, li.LinkInfo.InfoKind)
	}
	if li.LinkInfo.InfoData.Table == 0 {
		t.Fatalf("VRFTableID: %s has no associated table", vrf)
	}
	return li.LinkInfo.InfoData.Table
}

// AssertVethPairPresent fails the test if the veth pair created by
// SetupVethLeak is not wired up correctly: both ends must exist and be up,
// and veth-provider must be enslaved to vrf-provider.
func AssertVethPairPresent(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		def, defOK := readLink(t, VethDefaultName)
		prov, provOK := readLink(t, VethProviderName)
		if defOK && provOK {
			// Both ends must be UP. iproute2 sets operstate=UP once the
			// kernel has finished bringing the interface up.
			defUp := def.OperState == "UP" || hasFlag(def.Flags, "UP")
			provUp := prov.OperState == "UP" || hasFlag(prov.Flags, "UP")
			if defUp && provUp && prov.Master == DefaultVRFName {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("veth pair not fully present after %s: default=%+v provider=%+v",
				timeout, def, prov)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertNoVethPair fails the test if either side of the veth pair is still
// present past timeout. Used by teardown scenarios.
func AssertNoVethPair(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, defOK := readLink(t, VethDefaultName)
		_, provOK := readLink(t, VethProviderName)
		if !defOK && !provOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("veth pair still present after %s (default=%v, provider=%v)",
				timeout, defOK, provOK)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// AssertVethRouteInVRF fails the test if no per-network VRF route exists for
// cidr — gateway DefaultVethNexthop, dev veth-provider, proto
// VethLeakRouteProtocol, in the VRF's table. Polls up to timeout.
func AssertVethRouteInVRF(t *testing.T, cidr string, timeout time.Duration) {
	t.Helper()
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		t.Fatalf("AssertVethRouteInVRF: invalid CIDR %q: %v", cidr, err)
	}
	tableID := VRFTableID(t, DefaultVRFName)
	deadline := time.Now().Add(timeout)
	var lastOut string
	for {
		out, err := exec.Command("ip", "-j", "route", "show", "table", strconv.Itoa(tableID)).CombinedOutput()
		lastOut = string(out)
		if err == nil {
			var routes []routeInfo
			if err := json.Unmarshal(out, &routes); err == nil {
				for _, r := range routes {
					if r.Dst == cidr &&
						r.Gateway == DefaultVethNexthop &&
						r.Dev == VethProviderName &&
						protocolMatches(r.Protocol, VethLeakRouteProtocol) {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("veth-leak VRF route %s via %s dev %s proto %d table %d not found after %s (last: %q)",
				cidr, DefaultVethNexthop, VethProviderName, VethLeakRouteProtocol, tableID, timeout,
				strings.TrimSpace(lastOut))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// AssertNoVethRouteInVRF fails the test if a per-network VRF route for cidr
// (matching the agent's gateway/dev/protocol fingerprint) still exists past
// timeout.
func AssertNoVethRouteInVRF(t *testing.T, cidr string, timeout time.Duration) {
	t.Helper()
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		t.Fatalf("AssertNoVethRouteInVRF: invalid CIDR %q: %v", cidr, err)
	}
	tableID := VRFTableID(t, DefaultVRFName)
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ip", "-j", "route", "show", "table", strconv.Itoa(tableID)).CombinedOutput()
		if err == nil {
			var routes []routeInfo
			if err := json.Unmarshal(out, &routes); err == nil {
				present := false
				for _, r := range routes {
					if r.Dst == cidr &&
						r.Gateway == DefaultVethNexthop &&
						r.Dev == VethProviderName &&
						protocolMatches(r.Protocol, VethLeakRouteProtocol) {
						present = true
						break
					}
				}
				if !present {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("veth-leak VRF route %s in table %d still present after %s (out: %q)",
				cidr, tableID, timeout, strings.TrimSpace(string(out)))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// AssertVethDefaultRouteInLeakTable fails the test if the leak-table default
// route SetupVethLeak installs is missing. Format:
//
//	default via <DefaultVethProviderIP> dev veth-default table <DefaultVethLeakTableID>
func AssertVethDefaultRouteInLeakTable(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	for {
		out, err := exec.Command("ip", "-j", "route", "show", "table", strconv.Itoa(DefaultVethLeakTableID)).CombinedOutput()
		lastOut = string(out)
		if err == nil {
			var routes []routeInfo
			if err := json.Unmarshal(out, &routes); err == nil {
				for _, r := range routes {
					if r.Dst == "default" &&
						r.Gateway == DefaultVethProviderIP &&
						r.Dev == VethDefaultName {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("veth-leak default route in table %d not found after %s (last: %q)",
				DefaultVethLeakTableID, timeout, strings.TrimSpace(lastOut))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// AssertIPRuleAtPriority fails the test if no policy rule with the given
// priority and source CIDR pointing to DefaultVethLeakTableID exists. Polls
// up to timeout to give the agent time to install rules after a state change.
func AssertIPRuleAtPriority(t *testing.T, priority int, src string, timeout time.Duration) {
	t.Helper()
	AssertIPRuleAtPriorityTable(t, priority, src, DefaultVethLeakTableID, timeout)
}

// AssertIPRuleAtPriorityTable is the table-parameterised form of
// AssertIPRuleAtPriority. Scenarios that override veth_leak_table_id (see
// the table-collisions scenario in #88 item 2) need to assert against the
// configured table rather than the agent's default.
func AssertIPRuleAtPriorityTable(t *testing.T, priority int, src string, tableID int, timeout time.Duration) {
	t.Helper()
	if _, _, err := net.ParseCIDR(src); err != nil {
		t.Fatalf("AssertIPRuleAtPriorityTable: invalid CIDR %q: %v", src, err)
	}
	deadline := time.Now().Add(timeout)
	var lastOut string
	for {
		out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput()
		lastOut = string(out)
		if err == nil {
			var rules []ruleInfo
			if err := json.Unmarshal(out, &rules); err == nil {
				for _, r := range rules {
					if r.Priority == priority &&
						r.srcCIDR() == src &&
						tableMatches(r.Table, tableID) {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip rule priority %d from %s table %d not present after %s (last: %q)",
				priority, src, tableID, timeout, strings.TrimSpace(lastOut))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// AssertNoIPRuleAtPriority fails the test if any policy rule with the given
// priority and source CIDR remains past timeout. The mirror of
// AssertIPRuleAtPriority for cleanup checks.
func AssertNoIPRuleAtPriority(t *testing.T, priority int, src string, timeout time.Duration) {
	t.Helper()
	if _, _, err := net.ParseCIDR(src); err != nil {
		t.Fatalf("AssertNoIPRuleAtPriority: invalid CIDR %q: %v", src, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput()
		if err == nil {
			var rules []ruleInfo
			if err := json.Unmarshal(out, &rules); err == nil {
				present := false
				for _, r := range rules {
					if r.Priority == priority && r.srcCIDR() == src {
						present = true
						break
					}
				}
				if !present {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip rule priority %d from %s still present after %s (out: %q)",
				priority, src, timeout, strings.TrimSpace(string(out)))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// DeleteIPRule removes a policy rule matching priority + src CIDR. Used by
// the drift-recovery scenario to simulate an out-of-band operator action.
// Errors are surfaced via t.Fatalf — if the test cannot mutate state it
// cannot validate recovery.
func DeleteIPRule(t *testing.T, priority int, src string) {
	t.Helper()
	out, err := exec.Command("ip", "rule", "del", "priority", strconv.Itoa(priority), "from", src).CombinedOutput()
	if err != nil {
		t.Fatalf("ip rule del priority %d from %s: %v (%s)", priority, src, err, strings.TrimSpace(string(out)))
	}
}

// AssertIPRuleToPriorityTable waits for a destination policy rule at priority
// that points at tableID. This is used for the local-FIP exception placed just
// before the veth-leak source policy rule.
func AssertIPRuleToPriorityTable(t *testing.T, priority int, dst string, tableID int, timeout time.Duration) {
	t.Helper()
	if _, _, err := net.ParseCIDR(dst); err != nil {
		t.Fatalf("AssertIPRuleToPriorityTable: invalid CIDR %q: %v", dst, err)
	}
	deadline := time.Now().Add(timeout)
	var lastOut string
	for {
		out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput()
		lastOut = string(out)
		if err == nil {
			var rules []ruleInfo
			if err := json.Unmarshal(out, &rules); err == nil {
				for _, r := range rules {
					if r.Priority == priority && r.dstCIDR() == dst && tableMatches(r.Table, tableID) {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip rule priority %d to %s table %d not present after %s (last: %q)",
				priority, dst, tableID, timeout, strings.TrimSpace(lastOut))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// AssertNoIPRuleToPriority waits until no matching destination policy rule
// remains. It verifies FIP cleanup and that drift was actually injected.
func AssertNoIPRuleToPriority(t *testing.T, priority int, dst string, timeout time.Duration) {
	t.Helper()
	if _, _, err := net.ParseCIDR(dst); err != nil {
		t.Fatalf("AssertNoIPRuleToPriority: invalid CIDR %q: %v", dst, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput()
		if err == nil {
			var rules []ruleInfo
			if err := json.Unmarshal(out, &rules); err == nil {
				present := false
				for _, r := range rules {
					if r.Priority == priority && r.dstCIDR() == dst {
						present = true
						break
					}
				}
				if !present {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip rule priority %d to %s still present after %s (out: %q)",
				priority, dst, timeout, strings.TrimSpace(string(out)))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// DeleteIPRuleTo removes a destination policy rule from the main table. It
// simulates an upgraded host or an out-of-band deletion for reconciliation.
func DeleteIPRuleTo(t *testing.T, priority int, dst string) {
	t.Helper()
	out, err := exec.Command("ip", "rule", "del", "priority", strconv.Itoa(priority), "to", dst, "lookup", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("ip rule del priority %d to %s lookup main: %v (%s)", priority, dst, err, strings.TrimSpace(string(out)))
	}
}

// hasFlag reports whether flag appears in flags. Used to fall back to UP
// detection when iproute2 omits operstate (rare, but seen on some kernels
// with veth devices that have no carrier).
func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if f == flag {
			return true
		}
	}
	return false
}

// protocolMatches reports whether a route's protocol field (rendered by
// iproute2 as either an int or a string) corresponds to want. iproute2
// prints the numeric form unless the protocol has a name in
// /etc/iproute2/rt_protos.d/, so the agent's custom proto 44 normally
// renders as "44".
func protocolMatches(raw json.RawMessage, want int) bool {
	if len(raw) == 0 {
		return false
	}
	// Try int first.
	var asInt int
	if err := json.Unmarshal(raw, &asInt); err == nil {
		return asInt == want
	}
	// Fall back to string. iproute2 may emit the int as a JSON string on some
	// versions ("44"), or a registered name. We compare to the int form only.
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		if n, err := strconv.Atoi(asStr); err == nil && n == want {
			return true
		}
	}
	return false
}

// tableMatches reports whether a rule's table field matches want. iproute2
// renders the table as either an int or a registered name string ("main",
// "local", "default") — for the agent's custom table id 200 it is normally
// the int form.
func tableMatches(raw json.RawMessage, want int) bool {
	if len(raw) == 0 {
		return false
	}
	var asInt int
	if err := json.Unmarshal(raw, &asInt); err == nil {
		return asInt == want
	}
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		if n, err := strconv.Atoi(asStr); err == nil && n == want {
			return true
		}
	}
	return false
}

// scrubVethLeakState removes any veth-leak residue this test (or a previous
// failing test) may have left behind. Best-effort, idempotent — between
// scenarios this guarantees no stale veth pair, leak-table routes, or
// per-network policy rules can poison the next case.
//
// Acceptance criterion #3 of issue #56 is that no test leaks veth devices.
// scrubLocalState calls this so it runs before every scenario, regardless of
// whether the previous one tested veth-leak directly.
func scrubVethLeakState(t *testing.T) {
	t.Helper()

	// Drop policy rules we might have planted at the agent's priorities.
	// This includes both the veth-leak source rule and the local-FIP
	// destination exception immediately before it. Missing rules cause `ip
	// rule del` to exit non-zero with "RTNETLINK answers: No such file or
	// directory" — ignore.
	if out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput(); err == nil {
		var rules []ruleInfo
		if err := json.Unmarshal(out, &rules); err == nil {
			for _, r := range rules {
				switch {
				case r.Priority == DefaultVethLeakRulePriority && r.srcCIDR() != "":
					_ = exec.Command("ip", "rule", "del",
						"priority", strconv.Itoa(r.Priority),
						"from", r.srcCIDR(),
					).Run()
				case r.Priority == DefaultVethLeakRulePriority-1 && r.dstCIDR() != "":
					_ = exec.Command("ip", "rule", "del",
						"priority", strconv.Itoa(r.Priority),
						"to", r.dstCIDR(), "lookup", "main",
					).Run()
				}
			}
		}
	}

	// Flush the agent's leak table (default route + anything else that
	// might have been added) and delete the veth pair. Deleting one end
	// removes both, which also drops any remaining proto-44 routes the
	// kernel had pinned to the link.
	_ = exec.Command("ip", "route", "flush", "table", strconv.Itoa(DefaultVethLeakTableID)).Run()
	_ = exec.Command("ip", "link", "del", VethDefaultName).Run()
}
