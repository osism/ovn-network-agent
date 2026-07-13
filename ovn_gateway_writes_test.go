package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// captureSlog routes the default logger into a buffer for the duration of the
// test. No test in this package runs in parallel at the top level, so swapping
// the process-wide default is safe here.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// strPtr is a tiny helper for taking the address of a string literal.
func strPtr(s string) *string { return &s }

// findOps returns ops in the recorded transaction at index `transactIdx`
// that match the given Op verb and Table, in order.
func findOps(t *testing.T, transacts [][]ovsdb.Operation, transactIdx int, verb, table string) []ovsdb.Operation {
	t.Helper()
	if transactIdx >= len(transacts) {
		t.Fatalf("recorded only %d transacts, wanted index %d", len(transacts), transactIdx)
	}
	var matched []ovsdb.Operation
	for _, op := range transacts[transactIdx] {
		if op.Op == verb && op.Table == table {
			matched = append(matched, op)
		}
	}
	return matched
}

// =============================================================================
// ensureDefaultRoute
// =============================================================================

func TestEnsureDefaultRoute_CreatesNewWhenAbsent(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1",
		Name: "router1",
	})

	lr := LocalRouterInfo{
		RouterName: "router1",
		RouterUUID: "lr-uuid-1",
		LRPName:    "lrp-abc",
	}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 {
		t.Fatalf("expected 1 transact, got %d", len(tx))
	}
	if len(tx[0]) != 2 {
		t.Fatalf("expected 2 ops (insert + mutate), got %d: %+v", len(tx[0]), tx[0])
	}

	if got := findOps(t, tx, 0, ovsdb.OperationInsert, "Logical_Router_Static_Route"); len(got) != 1 {
		t.Errorf("expected 1 insert on Logical_Router_Static_Route, got %d (ops=%+v)", len(got), tx[0])
	}
	muts := findOps(t, tx, 0, ovsdb.OperationMutate, "Logical_Router")
	if len(muts) != 1 {
		t.Fatalf("expected 1 mutate on Logical_Router, got %d", len(muts))
	}
	mut := muts[0]
	if len(mut.Mutations) != 1 || mut.Mutations[0].Column != "static_routes" || mut.Mutations[0].Mutator != ovsdb.MutateOperationInsert {
		t.Errorf("unexpected mutate op: %+v", mut)
	}
	uuid, ok := mut.Mutations[0].Value.(ovsdb.UUID)
	if !ok || uuid.GoUUID != "new_route" {
		t.Errorf("mutate value should reference uuid-name 'new_route', got %#v", mut.Mutations[0].Value)
	}
}

func TestEnsureDefaultRoute_NoOpWhenAlreadyCorrect(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID:         "lr-uuid-1",
		Name:         "router1",
		StaticRoutes: []string{"route-uuid-1"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "route-uuid-1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.254",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-a",
		},
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %d: %+v", len(got), got)
	}
}

func TestEnsureDefaultRoute_UpdatesChassisTagAfterFailover(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1", Name: "router1", StaticRoutes: []string{"route-uuid-1"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "route-uuid-1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.254",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-b",
		},
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one op, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationUpdate || op.Table != "Logical_Router_Static_Route" || op.UUID != "route-uuid-1" {
		t.Errorf("expected update on route-uuid-1, got %+v", op)
	}
}

func TestEnsureDefaultRoute_UpdatesNexthopWhenWrong(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1", Name: "router1", StaticRoutes: []string{"route-uuid-1"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "route-uuid-1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.99", // stale nexthop
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-a",
		},
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one op, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationUpdate || op.UUID != "route-uuid-1" {
		t.Errorf("expected update on stale route, got %+v", op)
	}
}

func TestEnsureDefaultRoute_LeavesUnmanagedRouteAlone(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-uuid-1", Name: "router1", StaticRoutes: []string{"route-uuid-1"},
	})
	// Existing default route NOT managed by this agent (e.g., set by OpenStack).
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:        "route-uuid-1",
		IPPrefix:    "0.0.0.0/0",
		Nexthop:     "203.0.113.1",
		ExternalIDs: nil,
	})

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "lr-uuid-1", LRPName: "lrp-abc"}
	if err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254"); err != nil {
		t.Fatalf("ensureDefaultRoute: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts when an unmanaged default route exists, got %+v", got)
	}
}

func TestEnsureDefaultRoute_RouterNotFoundReturnsError(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	// No routers installed.
	_ = nb

	lr := LocalRouterInfo{RouterName: "router1", RouterUUID: "missing-uuid", LRPName: "lrp-abc"}
	err := c.ensureDefaultRoute(context.Background(), lr, "198.51.100.254")
	if err == nil {
		t.Fatal("expected error when router is missing, got nil")
	}
}

// =============================================================================
// ensureStaticMACBinding
// =============================================================================

func TestEnsureStaticMACBinding_CreatesWhenAbsent(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one insert, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationInsert || op.Table != "Static_MAC_Binding" {
		t.Errorf("expected insert on Static_MAC_Binding, got %+v", op)
	}
}

func TestEnsureStaticMACBinding_NoOpWhenCorrect(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Static_MAC_Binding", &NBStaticMACBinding{
		UUID:        "mb-1",
		LogicalPort: "lrp-abc",
		IP:          "198.51.100.254",
		MAC:         "aa:bb:cc:dd:ee:ff",
	})

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestEnsureStaticMACBinding_UpdatesOnFailover(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Static_MAC_Binding", &NBStaticMACBinding{
		UUID:        "mb-1",
		LogicalPort: "lrp-abc",
		IP:          "198.51.100.254",
		MAC:         "11:22:33:44:55:66", // stale MAC from previous owner
	})

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected one transact with one update, got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationUpdate || op.Table != "Static_MAC_Binding" || op.UUID != "mb-1" {
		t.Errorf("expected update on mb-1, got %+v", op)
	}
}

func TestEnsureStaticMACBinding_IgnoresOtherLRPs(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Static_MAC_Binding", &NBStaticMACBinding{
		UUID:        "mb-other",
		LogicalPort: "lrp-zzz",
		IP:          "198.51.100.254",
		MAC:         "aa:bb:cc:dd:ee:ff",
	})

	if err := c.ensureStaticMACBinding(context.Background(), "lrp-abc", "198.51.100.254", "aa:bb:cc:dd:ee:ff"); err != nil {
		t.Fatalf("ensureStaticMACBinding: %v", err)
	}
	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 || tx[0][0].Op != ovsdb.OperationInsert {
		t.Errorf("expected one insert (binding for other LRP must not match), got %+v", tx)
	}
}

// =============================================================================
// EnsureGatewayRouting
// =============================================================================

func TestEnsureGatewayRouting_SkipsRouterWithoutIPv4(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	routers := []LocalRouterInfo{{
		RouterName:  "router-v6",
		RouterUUID:  "lr-uuid-v6",
		LRPName:     "lrp-v6",
		LRPNetworks: []string{"fe80::1/64"}, // no IPv4 — virtualGatewayIP must fail
	}}
	macs := map[string]string{"lrp-v6": "aa:bb:cc:dd:ee:ff"}
	if err := c.EnsureGatewayRouting(context.Background(), routers, macs); err != nil {
		t.Fatalf("EnsureGatewayRouting: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts when no IPv4 CIDR present, got %+v", got)
	}
}

func TestEnsureGatewayRouting_ProcessesEachRouter(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-1", Name: "router1"},
		&NBLogicalRouter{UUID: "lr-2", Name: "router2"},
	)

	routers := []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-1", LRPNetworks: []string{"198.51.100.11/24"}},
		{RouterName: "router2", RouterUUID: "lr-2", LRPName: "lrp-2", LRPNetworks: []string{"203.0.113.1/24"}},
	}
	macs := map[string]string{
		"lrp-1": "aa:bb:cc:dd:ee:ff",
		"lrp-2": "aa:bb:cc:dd:ee:ff",
	}
	if err := c.EnsureGatewayRouting(context.Background(), routers, macs); err != nil {
		t.Fatalf("EnsureGatewayRouting: %v", err)
	}

	// Each router triggers one default-route create (insert+mutate, single transact)
	// and one MAC-binding insert (separate transact). 2 routers → 4 transacts.
	tx := nb.recordedTransacts()
	if len(tx) != 4 {
		t.Fatalf("expected 4 transacts (2 routers × {route, mac}), got %d: %+v", len(tx), tx)
	}
}

// TestEnsureGatewayRouting_SurfacesWriteFailure pins the fix for the
// always-nil return: when a router's OVSDB write fails, the failure is joined
// into a non-nil error so the caller's error log becomes reachable. The pass
// still processes both routers rather than aborting on the first failure.
func TestEnsureGatewayRouting_SurfacesWriteFailure(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-1", Name: "router1"},
		&NBLogicalRouter{UUID: "lr-2", Name: "router2"},
	)
	nb.transactErr = errors.New("connection refused")

	routers := []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-1", LRPNetworks: []string{"198.51.100.11/24"}},
		{RouterName: "router2", RouterUUID: "lr-2", LRPName: "lrp-2", LRPNetworks: []string{"203.0.113.1/24"}},
	}
	macs := map[string]string{
		"lrp-1": "aa:bb:cc:dd:ee:ff",
		"lrp-2": "aa:bb:cc:dd:ee:ff",
	}
	err := c.EnsureGatewayRouting(context.Background(), routers, macs)
	if err == nil {
		t.Fatal("EnsureGatewayRouting returned nil despite failing writes")
	}
	// Both routers should be named in the joined error, proving the pass did
	// not abort on the first failure.
	for _, want := range []string{"router1", "router2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %q does not mention %s", err, want)
		}
	}
}

// TestEnsureGatewayRouting_UsesPerRouterMAC verifies that each router's
// static MAC binding is written with that router's own segment interface
// MAC — two routers on different VLAN segments get two distinct bindings.
func TestEnsureGatewayRouting_UsesPerRouterMAC(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-1", Name: "router1"},
		&NBLogicalRouter{UUID: "lr-2", Name: "router2"},
	)

	routers := []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-1", LRPNetworks: []string{"198.51.100.11/24"}},
		{RouterName: "router2", RouterUUID: "lr-2", LRPName: "lrp-2", LRPNetworks: []string{"203.0.113.11/24"}},
	}
	macs := map[string]string{
		"lrp-1": "aa:bb:cc:dd:ee:65",
		"lrp-2": "aa:bb:cc:dd:ee:66",
	}
	if err := c.EnsureGatewayRouting(context.Background(), routers, macs); err != nil {
		t.Fatalf("EnsureGatewayRouting: %v", err)
	}

	// Collect the Static_MAC_Binding inserts across all transacts and check
	// each LRP got its own MAC. The fake's Create records only table +
	// UUIDName, so assert per-router write counts via transact shape: each
	// router produces one route transact (insert+mutate) and one MAC-binding
	// transact (single insert on Static_MAC_Binding).
	tx := nb.recordedTransacts()
	macInserts := 0
	for _, batch := range tx {
		for _, op := range batch {
			if op.Op == ovsdb.OperationInsert && op.Table == "Static_MAC_Binding" {
				macInserts++
			}
		}
	}
	if macInserts != 2 {
		t.Fatalf("expected 2 Static_MAC_Binding inserts (one per router), got %d: %+v", macInserts, tx)
	}
}

// TestEnsureGatewayRouting_UpdatesBindingToSegmentMAC drives the per-router
// MAC through an existing binding: router1's binding carries the old global
// bridge MAC and must be updated to its segment MAC, while router2's binding
// already matches its own segment MAC and must be left alone.
func TestEnsureGatewayRouting_UpdatesBindingToSegmentMAC(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-1", Name: "router1", StaticRoutes: []string{"rt-1"}},
		&NBLogicalRouter{UUID: "lr-2", Name: "router2", StaticRoutes: []string{"rt-2"}},
	)
	// Both default routes already exist and are correct, so the only writes
	// left are MAC-binding updates.
	nb.setRows("Logical_Router_Static_Route",
		&NBLogicalRouterStaticRoute{
			UUID: "rt-1", IPPrefix: "0.0.0.0/0", Nexthop: "198.51.100.254",
			ExternalIDs: map[string]string{"ovn-network-agent": "managed", "ovn-network-agent-chassis": "host-a"},
		},
		&NBLogicalRouterStaticRoute{
			UUID: "rt-2", IPPrefix: "0.0.0.0/0", Nexthop: "203.0.113.254",
			ExternalIDs: map[string]string{"ovn-network-agent": "managed", "ovn-network-agent-chassis": "host-a"},
		},
	)
	nb.setRows("Static_MAC_Binding",
		&NBStaticMACBinding{UUID: "mb-1", LogicalPort: "lrp-1", IP: "198.51.100.254", MAC: "aa:bb:cc:dd:ee:ff"},
		&NBStaticMACBinding{UUID: "mb-2", LogicalPort: "lrp-2", IP: "203.0.113.254", MAC: "aa:bb:cc:dd:ee:66"},
	)

	routers := []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-1", LRPNetworks: []string{"198.51.100.11/24"}},
		{RouterName: "router2", RouterUUID: "lr-2", LRPName: "lrp-2", LRPNetworks: []string{"203.0.113.11/24"}},
	}
	macs := map[string]string{
		"lrp-1": "aa:bb:cc:dd:ee:65",
		"lrp-2": "aa:bb:cc:dd:ee:66",
	}
	if err := c.EnsureGatewayRouting(context.Background(), routers, macs); err != nil {
		t.Fatalf("EnsureGatewayRouting: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Fatalf("expected exactly one update transact (router1's binding), got %+v", tx)
	}
	op := tx[0][0]
	if op.Op != ovsdb.OperationUpdate || op.Table != "Static_MAC_Binding" || op.UUID != "mb-1" {
		t.Fatalf("expected update on mb-1, got %+v", op)
	}
	if got := op.Row["mac"]; got != "aa:bb:cc:dd:ee:65" {
		t.Errorf("updated MAC = %v, want aa:bb:cc:dd:ee:65", got)
	}
}

// TestEnsureGatewayRouting_SkipsRouterWithoutMAC pins the per-router
// "no MAC → no write" contract: a router with an empty (or absent) MAC in
// macForLRP is skipped entirely — neither default route nor MAC binding —
// while other routers proceed.
func TestEnsureGatewayRouting_SkipsRouterWithoutMAC(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-1", Name: "router1"},
		&NBLogicalRouter{UUID: "lr-2", Name: "router2"},
	)

	routers := []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-1", LRPNetworks: []string{"198.51.100.11/24"}},
		{RouterName: "router2", RouterUUID: "lr-2", LRPName: "lrp-2", LRPNetworks: []string{"203.0.113.11/24"}},
	}
	// lrp-1 has no MAC at all; lrp-2 resolves normally.
	macs := map[string]string{"lrp-2": "aa:bb:cc:dd:ee:66"}
	if err := c.EnsureGatewayRouting(context.Background(), routers, macs); err != nil {
		t.Fatalf("EnsureGatewayRouting: %v", err)
	}

	// Only router2 writes: one route transact (insert+mutate) and one
	// MAC-binding transact. Nothing may reference router1.
	tx := nb.recordedTransacts()
	if len(tx) != 2 {
		t.Fatalf("expected 2 transacts (router2 only), got %d: %+v", len(tx), tx)
	}
	for _, batch := range tx {
		for _, op := range batch {
			if op.Op == ovsdb.OperationMutate && op.Table == "Logical_Router" {
				if uuid, ok := op.Where[0].Value.(ovsdb.UUID); !ok || uuid.GoUUID != "lr-2" {
					t.Errorf("route mutate must target lr-2 only, got %+v", op)
				}
			}
		}
	}
}

// =============================================================================
// EnsureActivePriorityLead
// =============================================================================

func TestEnsureActivePriorityLead(t *testing.T) {
	tests := []struct {
		name         string
		entries      []*NBGatewayChassis
		localRouters []LocalRouterInfo
		// wantBoosts maps the expected new priority for each LRP that should
		// be boosted. An empty map means no transact must be issued.
		wantBoosts map[string]int
	}{
		{
			name: "already leading with safe margin — no-op",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 3},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 2},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "boosts to outrank peer",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 5},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   map[string]int{"lrp-a": 6},
		},
		{
			name: "floors at minActivePriority when peer is drained",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 0},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   map[string]int{"lrp-a": minActivePriority},
		},
		{
			name: "no peers — no-op",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "no local entry — no-op",
			entries: []*NBGatewayChassis{
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 5},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "ignores LRPs not in local router set",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-other_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-other_host-b", ChassisName: "host-b", Priority: 5},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}},
			wantBoosts:   nil,
		},
		{
			name: "multiple LRPs needing boost are batched in a single transaction",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 4},
				{UUID: "g3", Name: "lrp-b_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g4", Name: "lrp-b_host-b", ChassisName: "host-b", Priority: 7},
			},
			localRouters: []LocalRouterInfo{{LRPName: "lrp-a"}, {LRPName: "lrp-b"}},
			wantBoosts:   map[string]int{"lrp-a": 5, "lrp-b": 8},
		},
		// LRP names with underscores hit the TrimSuffix derivation in
		// EnsureActivePriorityLead: the Gateway_Chassis row name is
		// {lrp_name}_{chassis_name}, so a router named `tenant_a_router` on
		// chassis `host-a` produces `tenant_a_router_host-a`, and the
		// suffix-trim must yield `tenant_a_router` — not a confused prefix
		// like `tenant_a` that would silently drop the chassis-priority
		// boost on multi-tenant deployments with snake_cased LRP names.
		{
			name: "LRP name with underscores still resolves under TrimSuffix",
			entries: []*NBGatewayChassis{
				{UUID: "g1", Name: "tenant_a_router_host-a", ChassisName: "host-a", Priority: 1},
				{UUID: "g2", Name: "tenant_a_router_host-b", ChassisName: "host-b", Priority: 5},
			},
			localRouters: []LocalRouterInfo{{LRPName: "tenant_a_router"}},
			wantBoosts:   map[string]int{"tenant_a_router": 6},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, nb, _ := newOVNClientWithFakes(t, "host-a")

			gws := make([]any, 0, len(tt.entries))
			for _, e := range tt.entries {
				gws = append(gws, e)
			}
			nb.setRows("Gateway_Chassis", gws...)

			err := c.EnsureActivePriorityLead(context.Background(), tt.localRouters, "host-a")
			if err != nil {
				t.Fatalf("EnsureActivePriorityLead: %v", err)
			}

			tx := nb.recordedTransacts()
			var updates []ovsdb.Operation
			for _, batch := range tx {
				for _, op := range batch {
					if op.Op == ovsdb.OperationUpdate && op.Table == "Gateway_Chassis" {
						updates = append(updates, op)
					}
				}
			}
			if len(tt.wantBoosts) == 0 {
				if len(updates) != 0 {
					t.Fatalf("expected no Gateway_Chassis updates, got %d: %+v", len(updates), updates)
				}
				return
			}

			if len(updates) != len(tt.wantBoosts) {
				t.Fatalf("expected %d update ops, got %d: %+v", len(tt.wantBoosts), len(updates), updates)
			}
			// Map each local UUID back to its LRP so we can assert the
			// computed priority for each update op.
			lrpByLocalUUID := make(map[string]string, len(tt.wantBoosts))
			for _, e := range tt.entries {
				if e.ChassisName != "host-a" {
					continue
				}
				lrp := e.Name[:len(e.Name)-len("_"+e.ChassisName)]
				if _, want := tt.wantBoosts[lrp]; want {
					lrpByLocalUUID[e.UUID] = lrp
				}
			}
			for _, op := range updates {
				lrp, ok := lrpByLocalUUID[op.UUID]
				if !ok {
					t.Errorf("update on unexpected UUID %q", op.UUID)
					continue
				}
				gotPrio, ok := op.Row["priority"].(int)
				if !ok {
					t.Errorf("op.Row[\"priority\"] missing or wrong type: %#v", op.Row)
					continue
				}
				if gotPrio != tt.wantBoosts[lrp] {
					t.Errorf("LRP %s: new priority = %d, want %d", lrp, gotPrio, tt.wantBoosts[lrp])
				}
				delete(lrpByLocalUUID, op.UUID)
			}
			if len(lrpByLocalUUID) > 0 {
				t.Errorf("expected updates for local UUIDs %v but they were not issued", lrpByLocalUUID)
			}
		})
	}
}

// TestEnsureActivePriorityLead_FallsBackToServerSelectOnCacheMiss covers the
// issue #115 race in the active-lead reconciler: with the local row missing
// from the cache, EnsureActivePriorityLead must recover it via a direct NB
// select and still boost the priority above the peer instead of silently
// skipping.
func TestEnsureActivePriorityLead_FallsBackToServerSelectOnCacheMiss(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	// Cache only has the peer row — the local Gateway_Chassis INSERT was
	// dropped between server and client.
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 5},
	)
	nb.setSelectRows("Gateway_Chassis", ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "g1"},
		"name":         "lrp-a_host-a",
		"chassis_name": "host-a",
		"priority":     float64(1),
	})

	err := c.EnsureActivePriorityLead(context.Background(),
		[]LocalRouterInfo{{LRPName: "lrp-a"}}, "host-a")
	if err != nil {
		t.Fatalf("EnsureActivePriorityLead: %v", err)
	}

	tx := nb.recordedTransacts()
	var updates []ovsdb.Operation
	for _, batch := range tx {
		for _, op := range batch {
			if op.Op == ovsdb.OperationUpdate && op.Table == "Gateway_Chassis" {
				updates = append(updates, op)
			}
		}
	}
	if len(updates) != 1 {
		t.Fatalf("expected one priority boost after select fallback, got %d: %+v", len(updates), updates)
	}
	if updates[0].UUID != "g1" {
		t.Errorf("update should target the recovered local row (g1), got %q", updates[0].UUID)
	}
	if got, ok := updates[0].Row["priority"].(int); !ok || got != 6 {
		t.Errorf("expected boost to 6 (max peer 5 + 1), got %#v", updates[0].Row["priority"])
	}
}

// gatewayChassisUpdates returns the Gateway_Chassis priority updates recorded
// on the fake NB client.
func gatewayChassisUpdates(tx [][]ovsdb.Operation) []ovsdb.Operation {
	var updates []ovsdb.Operation
	for _, batch := range tx {
		for _, op := range batch {
			if op.Op == ovsdb.OperationUpdate && op.Table == "Gateway_Chassis" {
				updates = append(updates, op)
			}
		}
	}
	return updates
}

// TestEnsureActivePriorityLead_SkipsBoostWhenCRPortMovedAway covers the
// anti-ratchet guard: the local Gateway_Chassis row trails the peer, so the
// reconcile snapshot would boost — but SB shows the chassisredirect port has
// already migrated to the peer. Boosting a no-longer-active chassis would
// create a higher-priority tie, so the boost must be skipped.
func TestEnsureActivePriorityLead_SkipsBoostWhenCRPortMovedAway(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
		&NBGatewayChassis{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 5},
	)
	sb.setRows("Chassis",
		&SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"},
		&SBChassis{UUID: "ch-b", Name: "ch-b", Hostname: "host-b"},
	)
	// The chassisredirect port for lrp-a is bound to host-b, not host-a.
	sb.setRows("Port_Binding", &SBPortBinding{
		UUID: "pb-1", LogicalPort: "cr-lrp-a", Type: "chassisredirect", Chassis: strPtr("ch-b"),
	})

	err := c.EnsureActivePriorityLead(context.Background(),
		[]LocalRouterInfo{{LRPName: "lrp-a", CRPort: "cr-lrp-a"}}, "host-a")
	if err != nil {
		t.Fatalf("EnsureActivePriorityLead: %v", err)
	}

	if got := gatewayChassisUpdates(nb.recordedTransacts()); len(got) != 0 {
		t.Errorf("must not boost when the chassisredirect port moved away, got %+v", got)
	}
}

// TestEnsureActivePriorityLead_BoostsWhenCRPortConfirmedLocal is the companion:
// the same trailing priority, but SB confirms the chassisredirect port is
// still bound here, so the boost proceeds.
func TestEnsureActivePriorityLead_BoostsWhenCRPortConfirmedLocal(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 1},
		&NBGatewayChassis{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 5},
	)
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})
	sb.setRows("Port_Binding", &SBPortBinding{
		UUID: "pb-1", LogicalPort: "cr-lrp-a", Type: "chassisredirect", Chassis: strPtr("ch-a"),
	})

	err := c.EnsureActivePriorityLead(context.Background(),
		[]LocalRouterInfo{{LRPName: "lrp-a", CRPort: "cr-lrp-a"}}, "host-a")
	if err != nil {
		t.Fatalf("EnsureActivePriorityLead: %v", err)
	}

	updates := gatewayChassisUpdates(nb.recordedTransacts())
	if len(updates) != 1 {
		t.Fatalf("expected one boost when the port is locally bound, got %d: %+v", len(updates), updates)
	}
	if got, ok := updates[0].Row["priority"].(int); !ok || got != 6 {
		t.Errorf("expected boost to 6 (max peer 5 + 1), got %#v", updates[0].Row["priority"])
	}
}

// =============================================================================
// RemoveManagedNBEntries
// =============================================================================

func TestRemoveManagedNBEntries_NoLocalRouters(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	if err := c.RemoveManagedNBEntries(context.Background()); err != nil {
		t.Fatalf("RemoveManagedNBEntries: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestRemoveManagedNBEntries_DeletesManagedRouteAndMACBinding(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	c.state.LocalRouters = []LocalRouterInfo{
		{RouterName: "router1", RouterUUID: "lr-1", LRPName: "lrp-abc"},
	}
	c.state.HasLocalRouters = true

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"route-managed", "route-foreign"},
	})
	nb.setRows("Logical_Router_Static_Route",
		&NBLogicalRouterStaticRoute{
			UUID:        "route-managed",
			IPPrefix:    "0.0.0.0/0",
			Nexthop:     "198.51.100.254",
			ExternalIDs: map[string]string{"ovn-network-agent": "managed"},
		},
		&NBLogicalRouterStaticRoute{
			UUID:     "route-foreign",
			IPPrefix: "10.0.0.0/8",
			Nexthop:  "203.0.113.1",
			// no managed tag → must be left alone
		},
	)
	nb.setRows("Static_MAC_Binding",
		&NBStaticMACBinding{UUID: "mb-local", LogicalPort: "lrp-abc", IP: "198.51.100.254", MAC: "aa:aa:aa:aa:aa:aa"},
		&NBStaticMACBinding{UUID: "mb-other", LogicalPort: "lrp-zzz", IP: "203.0.113.1", MAC: "bb:bb:bb:bb:bb:bb"},
	)

	if err := c.RemoveManagedNBEntries(context.Background()); err != nil {
		t.Fatalf("RemoveManagedNBEntries: %v", err)
	}

	tx := nb.writeTransacts()
	// Expect: one transact for the route (mutate + delete), one for the local MAC binding.
	if len(tx) != 2 {
		t.Fatalf("expected 2 transacts, got %d: %+v", len(tx), tx)
	}

	// First transact: route deletion (mutate router.static_routes + delete row).
	routeTx := tx[0]
	var sawMutate, sawDelete bool
	for _, op := range routeTx {
		switch op.Op {
		case ovsdb.OperationMutate:
			if op.Table == "Logical_Router" {
				sawMutate = true
				if len(op.Mutations) != 1 || op.Mutations[0].Mutator != ovsdb.MutateOperationDelete {
					t.Errorf("expected delete mutation on static_routes, got %+v", op.Mutations)
				}
			}
		case ovsdb.OperationDelete:
			if op.Table == "Logical_Router_Static_Route" && op.UUID == "route-managed" {
				sawDelete = true
			}
		}
	}
	if !sawMutate || !sawDelete {
		t.Errorf("route transact missing mutate or delete: %+v", routeTx)
	}

	// Second transact: MAC binding delete (only the one whose LRP is local).
	mbTx := tx[1]
	if len(mbTx) != 1 {
		t.Fatalf("expected exactly one MAC-binding op, got %+v", mbTx)
	}
	if mbTx[0].Op != ovsdb.OperationDelete || mbTx[0].Table != "Static_MAC_Binding" || mbTx[0].UUID != "mb-local" {
		t.Errorf("expected delete on mb-local, got %+v", mbTx[0])
	}
}

func TestRemoveManagedNBEntries_SkipsManagedRouteOnNonLocalRouter(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	c.state.LocalRouters = []LocalRouterInfo{
		{RouterName: "router-local", RouterUUID: "lr-local", LRPName: "lrp-local"},
	}
	c.state.HasLocalRouters = true

	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-local", Name: "router-local"},
		&NBLogicalRouter{UUID: "lr-remote", Name: "router-remote", StaticRoutes: []string{"route-remote"}},
	)
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID: "route-remote", IPPrefix: "0.0.0.0/0", Nexthop: "198.51.100.254",
		ExternalIDs: map[string]string{"ovn-network-agent": "managed"},
	})

	if err := c.RemoveManagedNBEntries(context.Background()); err != nil {
		t.Fatalf("RemoveManagedNBEntries: %v", err)
	}
	if got := nb.writeTransacts(); len(got) != 0 {
		t.Errorf("must not touch routes on non-local routers, got %+v", got)
	}
}

// =============================================================================
// CleanupStaleChassisManagedEntries
// =============================================================================

func TestCleanupStale_DeletesRouteAndCorrelatedMACBinding(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"route-stale"},
	})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:       "route-stale",
		IPPrefix:   "0.0.0.0/0",
		Nexthop:    "198.51.100.254",
		OutputPort: strPtr("lrp-abc"),
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-gone",
		},
	})
	nb.setRows("Static_MAC_Binding",
		&NBStaticMACBinding{UUID: "mb-correlated", LogicalPort: "lrp-abc", IP: "198.51.100.254", MAC: "aa:aa:aa:aa:aa:aa"},
		&NBStaticMACBinding{UUID: "mb-unrelated", LogicalPort: "lrp-other", IP: "10.0.0.1", MAC: "bb:bb:bb:bb:bb:bb"},
	)

	staleChassis := map[string]bool{"host-gone": true}
	if err := c.CleanupStaleChassisManagedEntries(context.Background(), staleChassis); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}

	tx := nb.writeTransacts()
	if len(tx) != 2 {
		t.Fatalf("expected 2 transacts (route + mac), got %d: %+v", len(tx), tx)
	}
	// First transact must delete the stale route.
	if got := findOps(t, tx, 0, ovsdb.OperationDelete, "Logical_Router_Static_Route"); len(got) != 1 || got[0].UUID != "route-stale" {
		t.Errorf("expected delete of route-stale in first transact, got %+v", tx[0])
	}
	// Second transact must delete only the correlated MAC binding (not mb-unrelated).
	if len(tx[1]) != 1 || tx[1][0].Op != ovsdb.OperationDelete || tx[1][0].UUID != "mb-correlated" {
		t.Errorf("expected delete of mb-correlated only, got %+v", tx[1])
	}
}

func TestCleanupStale_PreservesMACBindingWhenLiveChassisOwnsSamePort(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-1", Name: "router1", StaticRoutes: []string{"route-stale", "route-live"},
	})
	nb.setRows("Logical_Router_Static_Route",
		&NBLogicalRouterStaticRoute{
			UUID: "route-stale", IPPrefix: "0.0.0.0/0", Nexthop: "198.51.100.254",
			OutputPort: strPtr("lrp-abc"),
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-gone",
			},
		},
		&NBLogicalRouterStaticRoute{
			UUID: "route-live", IPPrefix: "10.0.0.0/8", Nexthop: "198.51.100.254",
			OutputPort: strPtr("lrp-abc"),
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-a", // live owner
			},
		},
	)
	nb.setRows("Static_MAC_Binding",
		&NBStaticMACBinding{UUID: "mb-shared", LogicalPort: "lrp-abc", IP: "198.51.100.254", MAC: "aa:aa:aa:aa:aa:aa"},
	)

	if err := c.CleanupStaleChassisManagedEntries(context.Background(), map[string]bool{"host-gone": true}); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}

	tx := nb.writeTransacts()
	// Stale route is deleted; MAC binding is preserved because lrp-abc is still live.
	if len(tx) != 1 {
		t.Fatalf("expected only the route-delete transact (no MAC binding delete), got %d: %+v", len(tx), tx)
	}
	if got := findOps(t, tx, 0, ovsdb.OperationDelete, "Logical_Router_Static_Route"); len(got) != 1 || got[0].UUID != "route-stale" {
		t.Errorf("expected delete of route-stale, got %+v", tx[0])
	}
}

func TestCleanupStale_SkipsLegacyRouteWithoutChassisTag(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr-1", Name: "router1", StaticRoutes: []string{"r1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:        "r1",
		IPPrefix:    "0.0.0.0/0",
		Nexthop:     "198.51.100.254",
		ExternalIDs: map[string]string{"ovn-network-agent": "managed"}, // no chassis tag
	})

	if err := c.CleanupStaleChassisManagedEntries(context.Background(), map[string]bool{"host-gone": true}); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}
	if got := nb.writeTransacts(); len(got) != 0 {
		t.Errorf("legacy untagged routes must be left alone, got %+v", got)
	}
}

func TestCleanupStale_SkipsUnmanagedRoutes(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr-1", Name: "router1", StaticRoutes: []string{"r1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "r1",
		IPPrefix: "0.0.0.0/0",
		Nexthop:  "198.51.100.254",
		ExternalIDs: map[string]string{
			"ovn-network-agent-chassis": "host-gone",
			// no "ovn-network-agent": "managed" key
		},
	})

	if err := c.CleanupStaleChassisManagedEntries(context.Background(), map[string]bool{"host-gone": true}); err != nil {
		t.Fatalf("CleanupStaleChassisManagedEntries: %v", err)
	}
	if got := nb.writeTransacts(); len(got) != 0 {
		t.Errorf("unmanaged routes must be left alone, got %+v", got)
	}
}

// =============================================================================
// DrainGateways
// =============================================================================

func TestSummarizeGatewayChassis(t *testing.T) {
	rows := func(n int) []NBGatewayChassis {
		list := make([]NBGatewayChassis, n)
		for i := range list {
			list[i] = NBGatewayChassis{
				Name:        fmt.Sprintf("lrp-%d_host-a", i),
				ChassisName: "host-a",
				Priority:    i,
			}
		}
		return list
	}

	tests := []struct {
		name      string
		list      []NBGatewayChassis
		want      int
		wantFirst string
		wantLast  string
	}{
		{name: "empty cache renders nothing", list: nil, want: 0},
		{
			name:      "short cache renders every row",
			list:      rows(3),
			want:      3,
			wantFirst: "lrp-0_host-a/host-a/0",
			wantLast:  "lrp-2_host-a/host-a/2",
		},
		{
			name:      "cache at the cap renders every row without a marker",
			list:      rows(maxLoggedCacheEntries),
			want:      maxLoggedCacheEntries,
			wantFirst: "lrp-0_host-a/host-a/0",
			wantLast:  fmt.Sprintf("lrp-%d_host-a/host-a/%d", maxLoggedCacheEntries-1, maxLoggedCacheEntries-1),
		},
		{
			// The NB monitor is table-wide, so a real deployment fills the
			// cache with every Gateway_Chassis row. Rendering all of them
			// produced a log line past journald's per-line limit.
			name:      "oversized cache is truncated with a remainder marker",
			list:      rows(maxLoggedCacheEntries + 5),
			want:      maxLoggedCacheEntries + 1,
			wantFirst: "lrp-0_host-a/host-a/0",
			wantLast:  "... (5 more)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := summarizeGatewayChassis(tc.list)
			if len(got) != tc.want {
				t.Fatalf("summarizeGatewayChassis(%d rows) returned %d entries, want %d: %v",
					len(tc.list), len(got), tc.want, got)
			}
			if tc.want == 0 {
				return
			}
			if got[0] != tc.wantFirst {
				t.Errorf("first entry = %q, want %q", got[0], tc.wantFirst)
			}
			if last := got[len(got)-1]; last != tc.wantLast {
				t.Errorf("last entry = %q, want %q", last, tc.wantLast)
			}
		})
	}
}

func TestDrainGateways_NothingToDrain(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	// Only entries for other chassis or already-drained.
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", ChassisName: "host-b", Priority: 5},
		&NBGatewayChassis{UUID: "g2", ChassisName: "host-a", Priority: 0},
	)
	_ = sb

	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestDrainGateways_BatchesAndCompletesWhenSBDrained(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
		&NBGatewayChassis{UUID: "g2", Name: "lrp-b_host-a", ChassisName: "host-a", Priority: 3},
		&NBGatewayChassis{UUID: "g3", Name: "lrp-c_host-a", ChassisName: "host-a", Priority: 2},
	)
	// SB shows no chassisredirect ports for host-a → first poll returns 0.
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})

	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 {
		t.Fatalf("expected one batched transact, got %d: %+v", len(tx), tx)
	}
	if len(tx[0]) != 3 {
		t.Fatalf("expected 3 update ops in the batch, got %d: %+v", len(tx[0]), tx[0])
	}
	for _, op := range tx[0] {
		if op.Op != ovsdb.OperationUpdate || op.Table != "Gateway_Chassis" {
			t.Errorf("unexpected op in drain batch: %+v", op)
		}
		if got, ok := op.Row["priority"].(int); !ok || got != 0 {
			t.Errorf("drain op should set priority=0, got %#v", op.Row["priority"])
		}
	}
}

func TestDrainGateways_TimeoutReturnsNilWithRemainingPorts(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
	)
	// SB has a chassisredirect port still bound to this chassis → polling
	// will not converge. With a tight ctx deadline the function must return
	// nil after the first poll observes >0 and the select fires ctx.Done.
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})
	sb.setRows("Port_Binding", &SBPortBinding{
		UUID:        "pb-1",
		LogicalPort: "cr-lrp-a",
		Type:        "chassisredirect",
		Chassis:     strPtr("ch-a"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := c.DrainGateways(ctx, "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("drain blocked too long: %v (ctx should fire promptly)", elapsed)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 1 || len(tx[0]) != 1 {
		t.Errorf("expected one batched priority update before timeout, got %+v", tx)
	}
}

// TestDrainGateways_FallsBackToServerSelectOnCacheMiss exercises the
// issue #115 race: the NB monitor cache is missing the local Gateway_Chassis
// row, so the cache scan finds nothing to drain. DrainGateways must fall
// back to a direct OVSDB select (which sees the server-side row) and lower
// the priority instead of silently no-op'ing.
func TestDrainGateways_FallsBackToServerSelectOnCacheMiss(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	// Cache has only peer entries — no row for host-a (the missed INSERT).
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 20},
		&NBGatewayChassis{UUID: "g3", Name: "lrp-a_host-c", ChassisName: "host-c", Priority: 10},
	)
	// Server-side state (visible to OperationSelect) still has the local row.
	nb.setSelectRows("Gateway_Chassis", ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "g1"},
		"name":         "lrp-a_host-a",
		"chassis_name": "host-a",
		// JSON-RPC numbers arrive as float64; reproduce that here so the
		// row decoder is exercised on the real wire shape.
		"priority": float64(30),
	})
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})

	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}

	tx := nb.recordedTransacts()
	if len(tx) != 2 {
		t.Fatalf("expected two transacts (select fallback + priority update), got %d: %+v", len(tx), tx)
	}
	// First transact is the OperationSelect against Gateway_Chassis.
	if len(tx[0]) != 1 || tx[0][0].Op != ovsdb.OperationSelect || tx[0][0].Table != "Gateway_Chassis" {
		t.Fatalf("expected first transact to be select on Gateway_Chassis, got %+v", tx[0])
	}
	if len(tx[0][0].Where) != 1 || tx[0][0].Where[0].Column != "chassis_name" || tx[0][0].Where[0].Value != "host-a" {
		t.Errorf("select must filter on chassis_name == host-a, got %+v", tx[0][0].Where)
	}
	// Second transact lowers the recovered row's priority to 0.
	updates := findOps(t, tx, 1, ovsdb.OperationUpdate, "Gateway_Chassis")
	if len(updates) != 1 {
		t.Fatalf("expected one priority update from the fallback, got %d: %+v", len(updates), tx[1])
	}
	if updates[0].UUID != "g1" {
		t.Errorf("update should target the row recovered from the server (g1), got %q", updates[0].UUID)
	}
	if got, ok := updates[0].Row["priority"].(int); !ok || got != 0 {
		t.Errorf("drain op must set priority=0, got %#v", updates[0].Row["priority"])
	}
}

// TestDrainGateways_EmptyMatchAfterCacheMissLogsServerRows covers the one
// branch where the "no entries to drain" log has to name what the SERVER
// returned: the cache missed the local row, the select fallback found it at
// priority 0, and nothing is left to drain. Dumping only the cache there is
// useless — the cache cannot hold the local row by construction, so a
// maintainer could not tell "NB has no row for this chassis" apart from "NB
// has it, at priority 0".
func TestDrainGateways_EmptyMatchAfterCacheMissLogsServerRows(t *testing.T) {
	logs := captureSlog(t)
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	// Cache has only peer entries — no row for host-a (the missed INSERT).
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 20},
	)
	// The server does hold the local row, but a previous drain left it at
	// priority 0, so filterDrainCandidates still yields nothing to drain.
	nb.setSelectRows("Gateway_Chassis", ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "g1"},
		"name":         "lrp-a_host-a",
		"chassis_name": "host-a",
		"priority":     float64(0),
	})

	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "drain: no gateway chassis entries to drain on this chassis") {
		t.Fatalf("expected the empty-match log line, got:\n%s", out)
	}
	if !strings.Contains(out, "server_count=1") {
		t.Errorf("empty-match log must report how many rows the NB select returned, got:\n%s", out)
	}
	if !strings.Contains(out, "lrp-a_host-a/host-a/0") {
		t.Errorf("empty-match log must render the server-side row that the cache lacked, got:\n%s", out)
	}
}

// TestDrainGateways_NoFallbackWhenLocalRowPresentAtZero asserts that an
// already-drained local row (priority 0) does NOT trigger the select
// fallback. The cache contains the row, so the existing "nothing to drain"
// fast path runs and no extra OVSDB chatter is emitted.
func TestDrainGateways_NoFallbackWhenLocalRowPresentAtZero(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 0},
	)

	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts (cache has the row), got %+v", got)
	}
}

// TestDrainGateways_FallbackReturnsErrorOnTransactFailure ensures the drain
// surfaces server-side errors from the select fallback instead of swallowing
// them silently — the caller (shutdown path) needs to record the failure.
func TestDrainGateways_FallbackReturnsErrorOnTransactFailure(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	// Cache has no local row, forcing the fallback.
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 20},
	)
	nb.transactErr = errors.New("connection refused")

	err := c.DrainGateways(context.Background(), "host-a")
	if err == nil || !strings.Contains(err.Error(), "NB select fallback failed") {
		t.Errorf("expected fallback error to be surfaced, got %v", err)
	}
}

// TestMarkTakeoverReady_WritesAndIsIdempotent covers the marker writer: a
// managed default route carrying a peer's marker is restamped for the local
// chassis in exactly one update that preserves the other external_ids, the
// re-run is a no-op once the marker already names this chassis, and a router
// with no managed default route is left untouched.
func TestMarkTakeoverReady_WritesAndIsIdempotent(t *testing.T) {
	t.Run("writes and preserves other keys, then idempotent", func(t *testing.T) {
		c, nb, _ := newOVNClientWithFakes(t, "host-a")
		nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", Name: "r1", StaticRoutes: []string{"sr1"}})
		nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
			UUID:     "sr1",
			IPPrefix: "0.0.0.0/0",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-b",
				takeoverReadyMarkerKey:      "host-b",
			},
		})
		localRouters := []LocalRouterInfo{{RouterName: "r1", RouterUUID: "lr1"}}

		if err := c.MarkTakeoverReady(context.Background(), localRouters); err != nil {
			t.Fatalf("MarkTakeoverReady: %v", err)
		}

		writes := nb.writeTransacts()
		if len(writes) != 1 {
			t.Fatalf("expected exactly one write transaction, got %d", len(writes))
		}
		var updates []ovsdb.Operation
		for _, op := range writes[0] {
			if op.Op == ovsdb.OperationUpdate && op.Table == "Logical_Router_Static_Route" {
				updates = append(updates, op)
			}
		}
		if len(updates) != 1 {
			t.Fatalf("expected one static-route update, got %d", len(updates))
		}
		ext, ok := updates[0].Row["external_ids"].(map[string]string)
		if !ok {
			t.Fatalf("update external_ids has type %T, want map[string]string", updates[0].Row["external_ids"])
		}
		if got := ext[takeoverReadyMarkerKey]; got != "host-a" {
			t.Errorf("advertised marker = %q, want host-a", got)
		}
		if ext["ovn-network-agent"] != "managed" || ext["ovn-network-agent-chassis"] != "host-b" {
			t.Errorf("marker write dropped existing external_ids: %v", ext)
		}

		// Re-run: the marker already names host-a, so no new write is issued.
		if err := c.MarkTakeoverReady(context.Background(), localRouters); err != nil {
			t.Fatalf("MarkTakeoverReady (rerun): %v", err)
		}
		if got := len(nb.writeTransacts()); got != 1 {
			t.Errorf("rerun issued a redundant write: total write transactions = %d, want 1", got)
		}
	})

	t.Run("no managed default route means no write", func(t *testing.T) {
		c, nb, _ := newOVNClientWithFakes(t, "host-a")
		nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", Name: "r1", StaticRoutes: []string{"sr1"}})
		// A default route that is NOT agent-managed (e.g. Neutron-configured).
		nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
			UUID:        "sr1",
			IPPrefix:    "0.0.0.0/0",
			ExternalIDs: map[string]string{},
		})

		if err := c.MarkTakeoverReady(context.Background(), []LocalRouterInfo{{RouterUUID: "lr1"}}); err != nil {
			t.Fatalf("MarkTakeoverReady: %v", err)
		}
		if got := len(nb.writeTransacts()); got != 0 {
			t.Errorf("marker written for a non-managed default route: %d write transactions, want 0", got)
		}
	})

	t.Run("empty local routers is a no-op", func(t *testing.T) {
		c, nb, _ := newOVNClientWithFakes(t, "host-a")
		if err := c.MarkTakeoverReady(context.Background(), nil); err != nil {
			t.Fatalf("MarkTakeoverReady(nil): %v", err)
		}
		if got := len(nb.recordedTransacts()); got != 0 {
			t.Errorf("empty local routers issued %d transactions, want 0", got)
		}
	})
}

// TestDrainGateways_EventSignalWakesMigrationWait verifies that a
// chassisredirect Port_Binding change ends the drain migration wait through
// drainWatchCh, without waiting for the safety re-poll tick. The settle delay
// is 0 so only the wait itself is measured; the CR port is unbound and the
// wake fired ~50ms in, far under the 1s drainRecheckInterval — so a prompt
// return proves the event, not the ticker, ended the wait.
func TestDrainGateways_EventSignalWakesMigrationWait(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	c.cfg.DrainSettleDelay = 0
	c.ready.Store(true)

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
	)
	// SB initially shows a chassisredirect port bound to host-a → first poll
	// returns 1, so the drain enters the wait.
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})
	sb.setRows("Port_Binding",
		&SBPortBinding{UUID: "pb1", Type: "chassisredirect", Chassis: strPtr("ch-a")},
	)

	// After the drain has entered the wait, unbind the port and wake it via a
	// chassisredirect signal. 50ms is well under drainRecheckInterval (1s), so
	// a prompt return can only come from the event, not the safety re-poll.
	go func() {
		time.Sleep(50 * time.Millisecond)
		sb.setRows("Port_Binding")
		c.signalDrainWatch()
	}()

	start := time.Now()
	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Errorf("drain returned after %v; expected the event to wake it well under the %v re-poll", elapsed, drainRecheckInterval)
	}
}

// TestAwaitTakeoverReady_ReturnsOnMarker verifies the reader half of the
// handshake: with a managed default route whose marker still names the local
// chassis, awaitTakeoverReady waits until the takeover chassis stamps its own
// name, then returns after only the small safety margin — well before a long
// drain deadline.
func TestAwaitTakeoverReady_ReturnsOnMarker(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.cfg.DrainSettleDelay = 50 * time.Millisecond
	c.state.LocalRouters = []LocalRouterInfo{{RouterUUID: "lr1"}}
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", StaticRoutes: []string{"sr1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:        "sr1",
		IPPrefix:    "0.0.0.0/0",
		ExternalIDs: map[string]string{"ovn-network-agent": "managed", takeoverReadyMarkerKey: "host-a"},
	})

	// Shortly after the wait starts, the takeover chassis stamps its name.
	go func() {
		time.Sleep(80 * time.Millisecond)
		nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
			UUID:        "sr1",
			IPPrefix:    "0.0.0.0/0",
			ExternalIDs: map[string]string{"ovn-network-agent": "managed", takeoverReadyMarkerKey: "host-b"},
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	c.awaitTakeoverReady(ctx, "host-a")
	elapsed := time.Since(start)
	if elapsed < c.cfg.DrainSettleDelay {
		t.Errorf("returned after %v, want at least the %v safety margin", elapsed, c.cfg.DrainSettleDelay)
	}
	if elapsed > 2*time.Second {
		t.Errorf("blocked %v; expected a prompt return on the marker, not the ctx deadline", elapsed)
	}
}

// TestAwaitTakeoverReady_TimeoutFallbackWhenMarkerNeverAppears verifies the
// clean fallback: when the marker never names a takeover chassis, the wait
// ends at the drain deadline and logs the fallback so cleanup still runs.
func TestAwaitTakeoverReady_TimeoutFallbackWhenMarkerNeverAppears(t *testing.T) {
	buf := captureSlog(t)
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.cfg.DrainSettleDelay = 50 * time.Millisecond
	c.state.LocalRouters = []LocalRouterInfo{{RouterUUID: "lr1"}}
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", StaticRoutes: []string{"sr1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:        "sr1",
		IPPrefix:    "0.0.0.0/0",
		ExternalIDs: map[string]string{"ovn-network-agent": "managed", takeoverReadyMarkerKey: "host-a"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	c.awaitTakeoverReady(ctx, "host-a")
	elapsed := time.Since(start)
	if elapsed < 250*time.Millisecond {
		t.Errorf("returned after %v, expected it to wait for the ~300ms deadline", elapsed)
	}
	if !strings.Contains(buf.String(), "takeover readiness marker not observed before timeout") {
		t.Errorf("expected timeout-fallback log line, got:\n%s", buf.String())
	}
}

// TestAwaitTakeoverReady_DisabledWhenSettleZero pins the "0 = no hold" escape
// hatch: a zero settle delay disables the whole handshake, so the wait returns
// immediately and reads nothing from NB.
func TestAwaitTakeoverReady_DisabledWhenSettleZero(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.cfg.DrainSettleDelay = 0
	c.state.LocalRouters = []LocalRouterInfo{{RouterUUID: "lr1"}}
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", StaticRoutes: []string{"sr1"}})
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:        "sr1",
		IPPrefix:    "0.0.0.0/0",
		ExternalIDs: map[string]string{"ovn-network-agent": "managed", takeoverReadyMarkerKey: "host-a"},
	})

	start := time.Now()
	c.awaitTakeoverReady(context.Background(), "host-a")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("settle delay 0 must disable the handshake, but it blocked %v", elapsed)
	}
	if got := len(nb.recordedTransacts()); got != 0 {
		t.Errorf("handshake disabled but read/wrote %d transactions, want 0", got)
	}
}

// TestAwaitTakeoverReady_NoManagedRouteHoldsMarginOnly covers the degraded
// path: when a local router has no agent-managed default route, no marker can
// ever appear, so the wait falls back to holding only the safety margin
// instead of polling to the deadline.
func TestAwaitTakeoverReady_NoManagedRouteHoldsMarginOnly(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.cfg.DrainSettleDelay = 60 * time.Millisecond
	c.state.LocalRouters = []LocalRouterInfo{{RouterUUID: "lr1"}}
	nb.setRows("Logical_Router", &NBLogicalRouter{UUID: "lr1", StaticRoutes: []string{"sr1"}})
	// Default route present but NOT agent-managed → excluded from the wait set.
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID: "sr1", IPPrefix: "0.0.0.0/0", ExternalIDs: map[string]string{},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	c.awaitTakeoverReady(ctx, "host-a")
	elapsed := time.Since(start)
	if elapsed < c.cfg.DrainSettleDelay {
		t.Errorf("returned after %v, want at least the %v margin", elapsed, c.cfg.DrainSettleDelay)
	}
	if elapsed > 2*time.Second {
		t.Errorf("no managed route must hold only the margin, not poll to the deadline; blocked %v", elapsed)
	}
}

// TestDrainGateways_SettleDelayHoldsBeforeReturn verifies the empty-wait-set
// path of the takeover handshake: once the SB shows all chassisredirect ports
// migrated away and there is no managed default route to watch (the test state
// has no local routers), DrainGateways holds for cfg.DrainSettleDelay before
// returning so the caller does not withdraw BGP routes before the takeover
// chassis is ready.
func TestDrainGateways_SettleDelayHoldsBeforeReturn(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	c.cfg.DrainSettleDelay = 80 * time.Millisecond

	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
	)
	// SB shows no chassisredirect ports for host-a → first poll returns 0.
	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})

	start := time.Now()
	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < c.cfg.DrainSettleDelay {
		t.Errorf("drain returned after %v, want at least the settle delay %v", elapsed, c.cfg.DrainSettleDelay)
	}
	if elapsed > 2*time.Second {
		t.Errorf("drain blocked too long: %v (settle delay is %v)", elapsed, c.cfg.DrainSettleDelay)
	}
}

// TestDrainGateways_SettleDelaySkippedWhenNothingToDrain ensures the handshake
// (and its margin) only run after an actual migration: when there is nothing
// to drain (non-HA / single-chassis / already drained) DrainGateways takes the
// no-op fast path and returns immediately even with a long settle delay
// configured.
func TestDrainGateways_SettleDelaySkippedWhenNothingToDrain(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	c.cfg.DrainSettleDelay = 30 * time.Second
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", ChassisName: "host-b", Priority: 5},
	)

	start := time.Now()
	if err := c.DrainGateways(context.Background(), "host-a"); err != nil {
		t.Fatalf("DrainGateways: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("drain held for %v with nothing to drain; settle delay must not apply", elapsed)
	}
}

// TestSelectLocalGatewayChassis_DecodesRowFields covers the rowToGatewayChassis
// decoder for the priority types it must accept: float64 (JSON wire format)
// and int (in-process fakes / mappers).
func TestSelectLocalGatewayChassis_DecodesRowFields(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setSelectRows("Gateway_Chassis",
		ovsdb.Row{
			"_uuid":        ovsdb.UUID{GoUUID: "g-float"},
			"name":         "lrp-a_host-a",
			"chassis_name": "host-a",
			"priority":     float64(7),
		},
		ovsdb.Row{
			"_uuid":        ovsdb.UUID{GoUUID: "g-int"},
			"name":         "lrp-b_host-a",
			"chassis_name": "host-a",
			"priority":     3,
		},
	)

	got, err := c.selectLocalGatewayChassis(context.Background(), "host-a")
	if err != nil {
		t.Fatalf("selectLocalGatewayChassis: %v", err)
	}
	want := []NBGatewayChassis{
		{UUID: "g-float", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 7},
		{UUID: "g-int", Name: "lrp-b_host-a", ChassisName: "host-a", Priority: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decoded rows mismatch:\n  got:  %+v\n  want: %+v", got, want)
	}
}

// =============================================================================
// RestoreDrainedGateways
// =============================================================================

func TestRestoreDrainedGateways_RestoresOnlyDrainedLocalEntries(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	nb.setRows("Gateway_Chassis",
		// drained local entries — must be restored to 1
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 0},
		&NBGatewayChassis{UUID: "g2", Name: "lrp-b_host-a", ChassisName: "host-a", Priority: 0},
		// already-active local entry — must be left alone
		&NBGatewayChassis{UUID: "g3", Name: "lrp-c_host-a", ChassisName: "host-a", Priority: 5},
		// drained entry on a different chassis — must be left alone
		&NBGatewayChassis{UUID: "g4", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 0},
	)

	c.RestoreDrainedGateways(context.Background(), "host-a")

	tx := nb.recordedTransacts()
	if len(tx) != 1 {
		t.Fatalf("expected one transact, got %d: %+v", len(tx), tx)
	}
	if len(tx[0]) != 2 {
		t.Fatalf("expected 2 restore ops, got %d: %+v", len(tx[0]), tx[0])
	}
	want := map[string]bool{"g1": true, "g2": true}
	for _, op := range tx[0] {
		if op.Op != ovsdb.OperationUpdate || op.Table != "Gateway_Chassis" {
			t.Errorf("unexpected op: %+v", op)
		}
		if !want[op.UUID] {
			t.Errorf("update on unexpected UUID %q", op.UUID)
		}
		if got, ok := op.Row["priority"].(int); !ok || got != 1 {
			t.Errorf("restore op should set priority=1, got %#v", op.Row["priority"])
		}
		delete(want, op.UUID)
	}
	if len(want) > 0 {
		t.Errorf("missing restore ops for %v", want)
	}
}

func TestRestoreDrainedGateways_NoOpWhenNothingDrained(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 5},
	)
	c.RestoreDrainedGateways(context.Background(), "host-a")
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts, got %+v", got)
	}
}

func TestRestoreDrainedGateways_ListErrorIsLoggedNotPanicking(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.listErr = errors.New("connection refused")

	// Must not panic; signature returns no error.
	c.RestoreDrainedGateways(context.Background(), "host-a")
	if got := nb.recordedTransacts(); len(got) != 0 {
		t.Errorf("expected no transacts on list error, got %+v", got)
	}
}

// TestRestoreDrainedGateways_FallsBackToServerSelectOnCacheMiss covers the
// issue #115 race on the startup restore path: the NB monitor cache is missing
// the local Gateway_Chassis row, so the cache scan finds nothing to restore.
// RestoreDrainedGateways must fall back to a direct OVSDB select and lift the
// drained row back to standby priority — otherwise the chassis stays at
// priority 0 and out of the HA group until a later restart happens to deliver
// a complete cache.
func TestRestoreDrainedGateways_FallsBackToServerSelectOnCacheMiss(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	// Cache has only a peer row — the local Gateway_Chassis INSERT was dropped.
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g2", Name: "lrp-a_host-b", ChassisName: "host-b", Priority: 1},
	)
	// Server-side state (visible to OperationSelect) still has the drained
	// local row.
	nb.setSelectRows("Gateway_Chassis", ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "g1"},
		"name":         "lrp-a_host-a",
		"chassis_name": "host-a",
		"priority":     float64(0),
	})

	c.RestoreDrainedGateways(context.Background(), "host-a")

	tx := nb.recordedTransacts()
	if len(tx) != 2 {
		t.Fatalf("expected two transacts (select fallback + restore update), got %d: %+v", len(tx), tx)
	}
	if len(tx[0]) != 1 || tx[0][0].Op != ovsdb.OperationSelect || tx[0][0].Table != "Gateway_Chassis" {
		t.Fatalf("expected first transact to be select on Gateway_Chassis, got %+v", tx[0])
	}
	if len(tx[0][0].Where) != 1 || tx[0][0].Where[0].Column != "chassis_name" || tx[0][0].Where[0].Value != "host-a" {
		t.Errorf("select must filter on chassis_name == host-a, got %+v", tx[0][0].Where)
	}
	updates := findOps(t, tx, 1, ovsdb.OperationUpdate, "Gateway_Chassis")
	if len(updates) != 1 {
		t.Fatalf("expected one restore update from the fallback, got %d: %+v", len(updates), tx[1])
	}
	if updates[0].UUID != "g1" {
		t.Errorf("update should target the row recovered from the server (g1), got %q", updates[0].UUID)
	}
	if got, ok := updates[0].Row["priority"].(int); !ok || got != 1 {
		t.Errorf("restore op must set priority=1, got %#v", updates[0].Row["priority"])
	}
}

// TestRestoreDrainedGateways_NoFallbackWhenLocalRowPresent asserts that a
// present local row suppresses the select fallback: the cache is trusted and
// no extra OVSDB select is emitted.
func TestRestoreDrainedGateways_NoFallbackWhenLocalRowPresent(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Gateway_Chassis",
		&NBGatewayChassis{UUID: "g1", Name: "lrp-a_host-a", ChassisName: "host-a", Priority: 0},
	)

	c.RestoreDrainedGateways(context.Background(), "host-a")

	for _, batch := range nb.recordedTransacts() {
		for _, op := range batch {
			if op.Op == ovsdb.OperationSelect {
				t.Errorf("unexpected select fallback while the cache has the local row: %+v", op)
			}
		}
	}
}

// =============================================================================
// ListManagedRouteChassis
// =============================================================================

func TestListManagedRouteChassis_CollectsTaggedChassis(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.setRows("Logical_Router_Static_Route",
		// Two managed entries, different chassis.
		&NBLogicalRouterStaticRoute{
			UUID:     "r1",
			IPPrefix: "0.0.0.0/0",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-a",
			},
		},
		&NBLogicalRouterStaticRoute{
			UUID:     "r2",
			IPPrefix: "0.0.0.0/0",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-b",
			},
		},
		// Duplicate chassis tag — should dedupe in the result.
		&NBLogicalRouterStaticRoute{
			UUID:     "r3",
			IPPrefix: "0.0.0.0/0",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-a",
			},
		},
		// Managed but with an empty chassis tag — must be skipped.
		&NBLogicalRouterStaticRoute{
			UUID:     "r4",
			IPPrefix: "0.0.0.0/0",
			ExternalIDs: map[string]string{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "",
			},
		},
		// Unmanaged route with a chassis tag — must be skipped.
		&NBLogicalRouterStaticRoute{
			UUID:     "r5",
			IPPrefix: "10.0.0.0/8",
			ExternalIDs: map[string]string{
				"ovn-network-agent-chassis": "host-c",
			},
		},
		// Route with nil ExternalIDs.
		&NBLogicalRouterStaticRoute{
			UUID:     "r6",
			IPPrefix: "192.0.2.0/24",
		},
	)

	got := c.ListManagedRouteChassis(context.Background())
	want := map[string]bool{"host-a": true, "host-b": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListManagedRouteChassis() = %v, want %v", got, want)
	}
}

func TestListManagedRouteChassis_EmptyOnNoRoutes(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	got := c.ListManagedRouteChassis(context.Background())
	if len(got) != 0 {
		t.Errorf("expected empty result on no routes, got %v", got)
	}
}

func TestListManagedRouteChassis_ListErrorReturnsNil(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.listErr = errors.New("connection refused")
	if got := c.ListManagedRouteChassis(context.Background()); got != nil {
		t.Errorf("expected nil on list error, got %v", got)
	}
}

// TestListManagedRouteChassis_RecoversDroppedRouteFromCache exercises the
// consistency guard on the stale-chassis scan: the monitor cache dropped a
// managed route, so its chassis tag would be missed. cachedList must detect
// the gap against the server and recover the route.
func TestListManagedRouteChassis_RecoversDroppedRouteFromCache(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")

	// Cache knows only the host-a route; the host-b route's INSERT was dropped.
	nb.setRows("Logical_Router_Static_Route", &NBLogicalRouterStaticRoute{
		UUID:     "r1",
		IPPrefix: "0.0.0.0/0",
		ExternalIDs: map[string]string{
			"ovn-network-agent":         "managed",
			"ovn-network-agent-chassis": "host-a",
		},
	})
	// The server has both routes.
	nb.setSelectRows("Logical_Router_Static_Route",
		ovsdb.Row{
			"_uuid":     ovsdb.UUID{GoUUID: "r1"},
			"ip_prefix": "0.0.0.0/0",
			"external_ids": ovsdb.OvsMap{GoMap: map[any]any{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-a",
			}},
		},
		ovsdb.Row{
			"_uuid":     ovsdb.UUID{GoUUID: "r2"},
			"ip_prefix": "0.0.0.0/0",
			"external_ids": ovsdb.OvsMap{GoMap: map[any]any{
				"ovn-network-agent":         "managed",
				"ovn-network-agent-chassis": "host-b",
			}},
		},
	)

	got := c.ListManagedRouteChassis(context.Background())
	want := map[string]bool{"host-a": true, "host-b": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListManagedRouteChassis() = %v, want %v (dropped route not recovered)", got, want)
	}
}

// =============================================================================
// transactOps error propagation (sanity check on the shared helper)
// =============================================================================

// TestTransactOpsSurfacesPerOperationErrors verifies the second error path
// in transactOps: when libovsdb's CheckOperationResults returns errors for
// individual operations, the helper returns the joined error so callers
// detect constraint violations even though Transact() succeeded.
func TestTransactOpsSurfacesPerOperationErrors(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	// Patch the fake to return an OperationResult with an Error string so
	// ovsdb.CheckOperationResults flags it.
	nb.opResults = []ovsdb.OperationResult{{Error: "constraint violation", Details: "duplicate row"}}

	err := c.transactOps(context.Background(), []ovsdb.Operation{
		{Op: ovsdb.OperationInsert, Table: "Logical_Router_Static_Route"},
	})
	if err == nil {
		t.Fatal("expected error when CheckOperationResults reports a per-op error")
	}
}

func TestTransactOpsPropagatesTransportError(t *testing.T) {
	c, nb, _ := newOVNClientWithFakes(t, "host-a")
	nb.transactErr = fmt.Errorf("connection lost")

	err := c.transactOps(context.Background(), []ovsdb.Operation{{Op: ovsdb.OperationUpdate, Table: "Gateway_Chassis"}})
	if err == nil || err.Error() != "connection lost" {
		t.Errorf("expected 'connection lost', got %v", err)
	}
}
