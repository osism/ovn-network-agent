//go:build integration

package integration

import (
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/osism/ovn-network-agent/test/integration/testenv"
)

// TestScenario_FailureInjection_RouteTableCollisions (#88 item 2):
//
// Three writers — kernel /32 routes for FIPs/VIPs, port-forward DNAT reply
// routing, veth-leak — each get a non-default routing table assigned via
// route_table_id / port_forward_table_id / veth_leak_table_id, and we
// verify nobody writes into a sibling's table.
//
// Provisioned state:
//   - one local router with LRP 198.51.100.11/24 (gives the veth-leak writer
//     a network to install per-network policy rules for)
//   - one FIP (gives the kernel-route writer a /32 to install)
//   - one port-forward VIP (gives the port-forward writer a default-route
//     entry to install in its reply table)
//
// Assertions go in two directions:
//
//  1. Each table contains exactly what its owner is expected to write:
//     200 holds the FIP /32, 201 holds the DNAT-reply default route, 202
//     holds the veth-leak default route plus a per-network policy rule
//     pointing at it.
//  2. No table contains entries from another writer. The FIP /32 cannot
//     leak into 201 or 202; the per-network rule cannot point at 200 or 201.
//
// The agent's setup path is order-sensitive (SetupVethLeak before
// SetupPortForward), so the test does not assert on install ordering — it
// waits for the steady-state convergence each writer is expected to reach.
func TestScenario_FailureInjection_RouteTableCollisions(t *testing.T) {
	ctx, cancel, nb, sb := startScenario(t)
	defer cancel()
	testenv.EnsureLoopback1(t)

	router := testenv.MakeLocalRouter(t, ctx, nb, sb, testenv.LocalRouterOpts{
		Name:        "tblcoll",
		LRPNetworks: []string{"198.51.100.11/24"},
	})

	const (
		fipExternal    = "198.51.100.42"
		pfVIP          = "198.51.100.71"
		providerNet    = "198.51.100.0/24"
		routeTable     = 200
		portForwardTbl = 201
		vethLeakTbl    = 202
	)
	testenv.AddFIP(t, ctx, nb, router, fipExternal, "10.0.0.42")

	cfg := testenv.FastDefaults()
	cfg.RouteTableID = intPtr(routeTable)
	cfg.PortForwardTableID = intPtr(portForwardTbl)
	cfg.VethLeakTableID = intPtr(vethLeakTbl)

	cfg.PortForwardDev = testenv.PortForwardLoopbackDev
	cfg.PortForwardCTZone = intPtr(64002)
	cfg.PortForwards = []testenv.PortForwardVIPFixture{{
		VIP:       pfVIP,
		ManageVIP: true,
		Rules: []testenv.PortForwardRuleFixture{{
			Proto: "tcp", Port: 80, DestAddr: "10.0.0.71",
		}},
	}}

	a := readyAgent(t, cfg)
	defer a.Stop(15 * time.Second)
	// SIGKILL-on-test-failure leaves residue in non-default tables; the
	// next scenario's scrubLocalState only flushes the harness defaults,
	// so scrub each configured table here.
	t.Cleanup(func() { testenv.ScrubPortForwardResidue(t) })
	t.Cleanup(func() {
		for _, id := range []int{routeTable, portForwardTbl, vethLeakTbl} {
			// Best-effort, idempotent.
			_ = flushIPTable(strconv.Itoa(id))
		}
	})

	routeTableStr := strconv.Itoa(routeTable)
	portForwardTblStr := strconv.Itoa(portForwardTbl)
	vethLeakTblStr := strconv.Itoa(vethLeakTbl)

	// === Positive: each writer placed its entries in its own table ====

	// Kernel-route writer: the FIP /32 and the PF VIP /32 land on br-ex
	// in table 200. iproute2 strips the /32 suffix for IPv4 in JSON output,
	// so dst is the bare IP.
	testenv.AssertRouteInTable(t, routeTableStr, fipExternal, testenv.DefaultBridgeDev, 20*time.Second)
	testenv.AssertRouteInTable(t, routeTableStr, pfVIP, testenv.DefaultBridgeDev, 20*time.Second)

	// Port-forward writer: a default route via veth-default in table 201.
	testenv.AssertRouteInTable(t, portForwardTblStr, "default", testenv.VethDefaultName, 20*time.Second)

	// Veth-leak writer: a default route via veth-default in table 202, plus
	// the per-network policy rule from 198.51.100.0/24 pointing at 202.
	testenv.AssertRouteInTable(t, vethLeakTblStr, "default", testenv.VethDefaultName, 20*time.Second)
	testenv.AssertIPRuleAtPriorityTable(t, testenv.DefaultVethLeakRulePriority,
		providerNet, vethLeakTbl, 20*time.Second)
	// The veth-default ingress exception (#265) is installed regardless of
	// the table layout and always points at main — never at 200, 201 or 202.
	testenv.AssertVethIngressRule(t, testenv.VethIngressRulePriority, 10*time.Second)

	// === Negative: no writer encroached on a sibling's table ===========

	// FIP /32 must NOT appear in the port-forward or veth-leak tables.
	testenv.AssertNoRouteInTable(t, portForwardTblStr, fipExternal, "", 1*time.Second)
	testenv.AssertNoRouteInTable(t, vethLeakTblStr, fipExternal, "", 1*time.Second)

	// PF VIP /32 likewise.
	testenv.AssertNoRouteInTable(t, portForwardTblStr, pfVIP, "", 1*time.Second)
	testenv.AssertNoRouteInTable(t, vethLeakTblStr, pfVIP, "", 1*time.Second)

	// The kernel-route table must NOT contain the default-via-veth-default
	// entry the other two writers planted in their own tables.
	testenv.AssertNoRouteInTable(t, routeTableStr, "default", testenv.VethDefaultName, 1*time.Second)
}

// flushIPTable is the test-side equivalent of `ip route flush table <id>`.
// Returns nil on success or on the harmless "table is empty" case. Used by
// the t.Cleanup chain in the route-table scenario so non-default tables do
// not leak into subsequent scenarios that assume the harness defaults.
func flushIPTable(table string) error {
	_, err := exec.Command("ip", "route", "flush", "table", table).CombinedOutput()
	return err
}
