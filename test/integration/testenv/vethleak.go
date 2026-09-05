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

	// VethIngressRulePriority is where the agent installs the veth-default
	// ingress exception rule (`iif veth-default lookup main`, #265): one
	// below the per-network leak rules, mirroring vethIngressRulePriority
	// in routing.go.
	VethIngressRulePriority = DefaultVethLeakRulePriority - 1

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
// rules: priority, source IP + prefix length, the incoming-interface selector
// of the ingress exception rule, and the destination table. iproute2 splits
// the CIDR across two fields (`src` is the bare IP, `srclen` is the prefix
// bits) — srcCIDR reassembles them. When there is no source match, iproute2
// emits `"src":"all"` and omits srclen; when there is no iif selector it omits
// `iif` altogether.
type ruleInfo struct {
	Priority int             `json:"priority"`
	Src      string          `json:"src,omitempty"`
	Srclen   int             `json:"srclen,omitempty"`
	Iif      string          `json:"iif,omitempty"`
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

// AssertVethIngressRule waits for the agent's veth-default ingress exception
// rule (#265): `iif veth-default lookup main` at the given priority. Polls up
// to timeout so a scenario can assert right after readiness or after a
// reconcile-driven repair.
func AssertVethIngressRule(t *testing.T, priority int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	for {
		out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput()
		lastOut = string(out)
		if err == nil {
			var rules []ruleInfo
			if err := json.Unmarshal(out, &rules); err == nil {
				for _, r := range rules {
					if r.Priority == priority && r.Iif == VethDefaultName && tableMatches(r.Table, 254) {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ip rule priority %d iif %s lookup main not present after %s (last: %q)",
				priority, VethDefaultName, timeout, strings.TrimSpace(lastOut))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// AssertNoVethIngressRule waits until no rule with the veth-default iif
// selector remains at any priority. The mirror of AssertVethIngressRule for
// the teardown, the disabled, and the drift-injection checks.
func AssertNoVethIngressRule(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	for {
		out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput()
		lastOut = string(out)
		if err == nil {
			var rules []ruleInfo
			if err := json.Unmarshal(out, &rules); err == nil {
				present := false
				for _, r := range rules {
					if r.Iif == VethDefaultName {
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
			t.Fatalf("ip rule iif %s still present after %s (out: %q)",
				VethDefaultName, timeout, strings.TrimSpace(lastOut))
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// DeleteVethIngressRule removes the ingress exception rule at priority out
// of band, the way an operator's `ip rule del` would, so a scenario can
// watch the next reconcile put it back. Errors surface via t.Fatalf — a
// drift the test could not inject would make the recovery assertion pass
// trivially.
func DeleteVethIngressRule(t *testing.T, priority int) {
	t.Helper()
	out, err := exec.Command("ip", "rule", "del",
		"priority", strconv.Itoa(priority), "iif", VethDefaultName, "lookup", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("ip rule del priority %d iif %s lookup main: %v (%s)",
			priority, VethDefaultName, err, strings.TrimSpace(string(out)))
	}
}

// linkStats is the subset of `ip -j -s link show <name>` the loop check
// reads: the 64-bit transmit packet counter.
type linkStats struct {
	Stats64 struct {
		TX struct {
			Packets uint64 `json:"packets"`
		} `json:"tx"`
	} `json:"stats64"`
}

// VethLinkTxPackets returns the TX packet counter of the named link. The
// veth-default counter is how TestScenario_VethLeakIngressNoLoop tells a
// packet that left the veth pair once from one that bounced between its
// ends until its TTL ran out.
func VethLinkTxPackets(t *testing.T, name string) uint64 {
	t.Helper()
	out, err := exec.Command("ip", "-j", "-s", "link", "show", name).CombinedOutput()
	if err != nil {
		t.Fatalf("ip -j -s link show %s: %v (%s)", name, err, strings.TrimSpace(string(out)))
	}
	var links []linkStats
	if err := json.Unmarshal(out, &links); err != nil || len(links) == 0 {
		t.Fatalf("ip -j -s link show %s: cannot decode stats64 from %q (err=%v)", name, strings.TrimSpace(string(out)), err)
	}
	return links[0].Stats64.TX.Packets
}

// PingFromVRF sends count ICMP echo requests from inside vrf-provider with
// the given source address, which it adds to the VRF device for the duration
// of the call. That is exactly the packet a peer gateway's VRF hands over
// through the veth pair: it leaves via veth-provider, arrives on veth-default,
// and its source sits inside the leaked provider prefix (#265). Nothing
// answers behind br-ex in the harness, so the ping's exit status is ignored;
// callers read the veth-default TX counter instead.
//
// The VRF socket context needs `ip vrf exec` (cgroup v2 + BPF). A harness
// kernel that cannot provide it skips the caller rather than failing it: the
// rule lifecycle is asserted elsewhere, only the forwarding proof needs this.
func PingFromVRF(t *testing.T, src, dst string, count int) {
	t.Helper()
	if out, err := exec.Command("ip", "vrf", "exec", DefaultVRFName, "true").CombinedOutput(); err != nil {
		t.Skipf("ip vrf exec %s is not available on this host: %v (%s)", DefaultVRFName, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("ip", "addr", "add", src+"/32", "dev", DefaultVRFName).CombinedOutput(); err != nil {
		t.Fatalf("ip addr add %s/32 dev %s: %v (%s)", src, DefaultVRFName, err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		_ = exec.Command("ip", "addr", "del", src+"/32", "dev", DefaultVRFName).Run()
	})
	out, err := exec.Command("ip", "vrf", "exec", DefaultVRFName,
		"ping", "-I", src, "-c", strconv.Itoa(count), "-W", "1", dst).CombinedOutput()
	t.Logf("ping from %s to %s in %s (exit=%v): %s", src, dst, DefaultVRFName, err, strings.TrimSpace(string(out)))
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
		// iproute2 prints the reserved tables by the names in
		// /etc/iproute2/rt_tables, never by number; the agent's ingress
		// exception rule points at main.
		if id, ok := reservedTables[asStr]; ok {
			return id == want
		}
		if n, err := strconv.Atoi(asStr); err == nil && n == want {
			return true
		}
	}
	return false
}

// reservedTables maps the routing-table names iproute2 prints for the
// kernel's reserved tables back to their ids (rt_tables(5)).
var reservedTables = map[string]int{
	"local":   255,
	"main":    254,
	"default": 253,
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
	// Both default and any custom priority a future test may inject get
	// flushed; missing rules cause `ip rule del` to exit non-zero with
	// "RTNETLINK answers: No such file or directory" — ignore.
	if out, err := exec.Command("ip", "-j", "rule", "show").CombinedOutput(); err == nil {
		var rules []ruleInfo
		if err := json.Unmarshal(out, &rules); err == nil {
			for _, r := range rules {
				// The ingress exception rule (#265) carries the veth-default
				// iif selector; drop it at whatever priority a previous
				// scenario's agent left it.
				if r.Iif == VethDefaultName {
					_ = exec.Command("ip", "rule", "del",
						"priority", strconv.Itoa(r.Priority),
						"iif", VethDefaultName,
					).Run()
					continue
				}
				if r.Priority != DefaultVethLeakRulePriority {
					continue
				}
				cidr := r.srcCIDR()
				if cidr == "" {
					continue
				}
				_ = exec.Command("ip", "rule", "del",
					"priority", strconv.Itoa(r.Priority),
					"from", cidr,
				).Run()
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
