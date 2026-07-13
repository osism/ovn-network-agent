package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// TestCachedListNoDrift: when the monitor cache and the server agree, cachedList
// returns the cache snapshot untouched (no recovery, no decode).
func TestCachedListNoDrift(t *testing.T) {
	_, _, sb := newOVNClientWithFakes(t, "host-a")
	chA := "ch-a"
	sb.setRows("Port_Binding",
		&SBPortBinding{UUID: "pb-1", LogicalPort: "cr-lrp-1", Type: "chassisredirect", Chassis: &chA},
		&SBPortBinding{UUID: "pb-2", LogicalPort: "cr-lrp-2", Type: "chassisredirect", Chassis: &chA},
	)

	got, err := cachedList(context.Background(), sb, "Port_Binding",
		pbCheckColumns, keyOfSBPortBinding, decodeSBPortBinding)
	if err != nil {
		t.Fatalf("cachedList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].LogicalPort != "cr-lrp-1" || got[1].LogicalPort != "cr-lrp-2" {
		t.Errorf("cache snapshot not preserved: %+v", got)
	}
}

// TestCachedListRecoversDroppedRow is the core regression test for issue #115:
// the monitor cache is missing a row that the server has. cachedList must
// detect the gap via the direct select and rebuild the full slice.
func TestCachedListRecoversDroppedRow(t *testing.T) {
	_, _, sb := newOVNClientWithFakes(t, "host-a")

	// Cache only knows about pb-1 — pb-2's INSERT was dropped.
	chA := "ch-a"
	sb.setRows("Port_Binding",
		&SBPortBinding{UUID: "pb-1", LogicalPort: "cr-lrp-1", Type: "chassisredirect", Chassis: &chA},
	)
	// The server has both rows.
	sb.setSelectRows("Port_Binding",
		ovsdb.Row{
			"_uuid":        ovsdb.UUID{GoUUID: "pb-1"},
			"logical_port": "cr-lrp-1",
			"type":         "chassisredirect",
			"chassis":      ovsdb.UUID{GoUUID: "ch-a"},
		},
		ovsdb.Row{
			"_uuid":        ovsdb.UUID{GoUUID: "pb-2"},
			"logical_port": "cr-lrp-2",
			"type":         "chassisredirect",
			"chassis":      ovsdb.UUID{GoUUID: "ch-a"},
		},
	)

	got, err := cachedList(context.Background(), sb, "Port_Binding",
		pbCheckColumns, keyOfSBPortBinding, decodeSBPortBinding)
	if err != nil {
		t.Fatalf("cachedList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (cache gap not recovered)", len(got))
	}
	byUUID := map[string]SBPortBinding{}
	for _, p := range got {
		byUUID[p.UUID] = p
	}
	pb2, ok := byUUID["pb-2"]
	if !ok {
		t.Fatal("recovered slice is missing the dropped row pb-2")
	}
	if pb2.LogicalPort != "cr-lrp-2" || pb2.Type != "chassisredirect" {
		t.Errorf("pb-2 decoded incorrectly: %+v", pb2)
	}
	if pb2.Chassis == nil || *pb2.Chassis != "ch-a" {
		t.Errorf("pb-2.Chassis = %v, want ch-a", pb2.Chassis)
	}
}

// TestAuthoritativeListServerSelectErrorTrustsCache: a failing direct select
// must degrade to the monitor cache rather than stall reconciliation.
func TestAuthoritativeListServerSelectErrorTrustsCache(t *testing.T) {
	_, _, sb := newOVNClientWithFakes(t, "host-a")
	sb.transactErr = errors.New("connection refused")

	cached := []SBChassis{{UUID: "ch-1", Name: "ch-1", Hostname: "host-a"}}
	got := authoritativeList(context.Background(), sb, "Chassis", chCheckColumns, cached,
		keyOfSBChassis, decodeSBChassis)
	if !reflect.DeepEqual(got, cached) {
		t.Errorf("got %+v, want cache snapshot %+v on select error", got, cached)
	}
}

// blockingTransactClient models an OVSDB server that is reachable at the TCP
// level but never answers — a SIGSTOP'd ovsdb-server or a blackholed network.
// Under client.WithReconnect a real Transact blocks on its context in exactly
// this way instead of returning an error.
type blockingTransactClient struct {
	*fakeOVSDBClient
}

func (b *blockingTransactClient) Transact(ctx context.Context, _ ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAuthoritativeListBoundsStuckServerSelect: a select against an
// unresponsive server must degrade to the monitor cache within
// consistencySelectTimeout instead of blocking on the caller's context. An
// unbounded select stalls reconciliation for the whole outage, and — because
// a blocked Transact holds the libovsdb client mutex read-side — blocks the
// inactivity probe's Disconnect, so the client can never start reconnecting.
// TestScenario_OVNReconnectAfterLongOutage pins the full path in CI.
func TestAuthoritativeListBoundsStuckServerSelect(t *testing.T) {
	_, _, sb := newOVNClientWithFakes(t, "host-a")
	blocked := &blockingTransactClient{fakeOVSDBClient: sb}

	old := consistencySelectTimeout
	consistencySelectTimeout = 100 * time.Millisecond
	t.Cleanup(func() { consistencySelectTimeout = old })

	cached := []SBChassis{{UUID: "ch-1", Name: "ch-1", Hostname: "host-a"}}
	done := make(chan []SBChassis, 1)
	go func() {
		done <- authoritativeList(context.Background(), blocked, "Chassis", chCheckColumns,
			cached, keyOfSBChassis, decodeSBChassis)
	}()
	select {
	case got := <-done:
		if !reflect.DeepEqual(got, cached) {
			t.Errorf("got %+v, want cache snapshot %+v on stuck select", got, cached)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authoritativeList still blocked after 5s: the consistency select is not bounded")
	}
}

// TestRefreshStateRecoversDroppedNATFromCache exercises the fix end-to-end:
// the NB monitor cache dropped the FIP's NAT row. Without the consistency
// guard the FIP would silently vanish from the desired set (and ensureRoutes
// would withdraw its /32). With the guard, refreshState recovers it.
func TestRefreshStateRecoversDroppedNATFromCache(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})
	chA := "ch-a"
	sb.setRows("Port_Binding", &SBPortBinding{
		UUID: "pb-1", LogicalPort: "cr-lrp-local", Type: "chassisredirect", Chassis: &chA,
	})
	nb.setRows("Logical_Router_Port", &NBLogicalRouterPort{
		UUID: "lrp-uuid-local", Name: "lrp-local",
		MAC: "fa:16:3e:aa:aa:aa", Networks: []string{"198.51.100.1/24"},
	})
	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-local", Name: "router-local",
		Ports: []string{"lrp-uuid-local"}, Nat: []string{"nat-fip", "nat-snat"},
	})

	// The NB cache is missing nat-fip — only nat-snat made it in.
	nb.setRows("NAT", &NBNAT{UUID: "nat-snat", Type: "snat", ExternalIP: "198.51.100.51"})
	// The server, however, has both NAT rows.
	nb.setSelectRows("NAT",
		ovsdb.Row{"_uuid": ovsdb.UUID{GoUUID: "nat-fip"}, "type": "dnat_and_snat", "external_ip": "198.51.100.50"},
		ovsdb.Row{"_uuid": ovsdb.UUID{GoUUID: "nat-snat"}, "type": "snat", "external_ip": "198.51.100.51"},
	)

	c.state.LocalChassisName = "host-a"
	c.refreshState(context.Background())
	snap := c.GetState()

	// The FIP's presence in NATIPToRouterMAC proves the dropped NAT row was
	// recovered from the direct select (a missing row would leave the key
	// absent, yielding the empty string).
	if snap.NATIPToRouterMAC["198.51.100.50"] != "fa:16:3e:aa:aa:aa" {
		t.Fatalf("NATIPToRouterMAC[FIP] = %q, want fa:16:3e:aa:aa:aa — dropped NAT row not recovered",
			snap.NATIPToRouterMAC["198.51.100.50"])
	}
}

// TestAuthoritativeListRecoversStaleColumn is the regression test for a
// dropped UPDATE: the monitor cache holds the right row (same _uuid) but a
// stale column value. A _uuid-set comparison sees no drift; the content-key
// check must catch it and rebuild the row from a direct select.
func TestAuthoritativeListRecoversStaleColumn(t *testing.T) {
	_, _, sb := newOVNClientWithFakes(t, "host-a")

	// Cache holds the chassisredirect port bound to ch-a.
	chA := "ch-a"
	cached := []SBPortBinding{
		{UUID: "pb-1", LogicalPort: "cr-lrp-1", Type: "chassisredirect", Chassis: &chA},
	}
	// The server has the same row (same _uuid) but bound to ch-b — a failover
	// UPDATE the monitor cache dropped.
	sb.setSelectRows("Port_Binding", ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "pb-1"},
		"logical_port": "cr-lrp-1",
		"type":         "chassisredirect",
		"chassis":      ovsdb.UUID{GoUUID: "ch-b"},
	})

	got := authoritativeList(context.Background(), sb, "Port_Binding", pbCheckColumns, cached,
		keyOfSBPortBinding, decodeSBPortBinding)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Chassis == nil || *got[0].Chassis != "ch-b" {
		t.Errorf("stale chassis column not recovered: got %v, want ch-b", got[0].Chassis)
	}
}

// TestRefreshStateRecoversStaleChassisBinding exercises the fix end-to-end: the
// SB monitor cache shows the chassisredirect port bound to a peer (the router
// looks non-local), but the server has since migrated it to this chassis. With
// only a _uuid check the stale binding would be trusted and the FIP dropped
// from the desired set; the content-key check recovers it.
func TestRefreshStateRecoversStaleChassisBinding(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	chB := "ch-b"
	sb.setRows("Chassis",
		&SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"},
		&SBChassis{UUID: "ch-b", Name: "ch-b", Hostname: "host-b"},
	)
	// Cache view: the chassisredirect port is bound to host-b — not local.
	sb.setRows("Port_Binding", &SBPortBinding{
		UUID: "pb-1", LogicalPort: "cr-lrp-local", Type: "chassisredirect", Chassis: &chB,
	})
	// Server view: the same row (same _uuid) has migrated to host-a.
	sb.setSelectRows("Port_Binding", ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "pb-1"},
		"logical_port": "cr-lrp-local",
		"type":         "chassisredirect",
		"chassis":      ovsdb.UUID{GoUUID: "ch-a"},
	})
	nb.setRows("Logical_Router_Port", &NBLogicalRouterPort{
		UUID: "lrp-uuid-local", Name: "lrp-local",
		MAC: "fa:16:3e:aa:aa:aa", Networks: []string{"198.51.100.1/24"},
	})
	nb.setRows("Logical_Router", &NBLogicalRouter{
		UUID: "lr-local", Name: "router-local",
		Ports: []string{"lrp-uuid-local"}, Nat: []string{"nat-fip"},
	})
	nb.setRows("NAT", &NBNAT{UUID: "nat-fip", Type: "dnat_and_snat", ExternalIP: "198.51.100.50"})

	c.state.LocalChassisName = "host-a"
	c.refreshState(context.Background())
	snap := c.GetState()

	if !snap.HasLocalRouters {
		t.Fatal("HasLocalRouters = false — stale chassisredirect binding not recovered")
	}
	if _, ok := snap.NATIPToRouterMAC["198.51.100.50"]; !ok {
		t.Fatalf("NATIPToRouterMAC = %v, missing FIP 198.51.100.50", snap.NATIPToRouterMAC)
	}
}

func TestDecodeNBLogicalRouter(t *testing.T) {
	// ports: multi-element set; nat: bare single UUID.
	row := ovsdb.Row{
		"_uuid": ovsdb.UUID{GoUUID: "lr-1"},
		"name":  "router-1",
		"ports": ovsdb.OvsSet{GoSet: []any{
			ovsdb.UUID{GoUUID: "p1"}, ovsdb.UUID{GoUUID: "p2"},
		}},
		"nat": ovsdb.UUID{GoUUID: "nat-1"},
	}
	got := decodeNBLogicalRouter(row)
	want := NBLogicalRouter{
		UUID: "lr-1", Name: "router-1",
		Ports: []string{"p1", "p2"}, Nat: []string{"nat-1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decodeNBLogicalRouter = %+v, want %+v", got, want)
	}
}

func TestDecodeSBPortBindingSetsAndMaps(t *testing.T) {
	row := ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "pb-1"},
		"type":         "patch",
		"logical_port": "external-port",
		"options":      ovsdb.OvsMap{GoMap: map[any]any{"peer": "lrp-local"}},
		"external_ids": ovsdb.OvsMap{GoMap: map[any]any{"neutron:device_owner": "network:router_gateway"}},
		"nat_addresses": ovsdb.OvsSet{GoSet: []any{
			"fa:16:3e:11:22:33 198.51.100.60",
		}},
		// chassis omitted entirely — an unbound port.
	}
	got := decodeSBPortBinding(row)
	if got.Options["peer"] != "lrp-local" {
		t.Errorf("Options = %v", got.Options)
	}
	if got.ExternalIDs["neutron:device_owner"] != "network:router_gateway" {
		t.Errorf("ExternalIDs = %v", got.ExternalIDs)
	}
	if len(got.NatAddresses) != 1 || got.NatAddresses[0] != "fa:16:3e:11:22:33 198.51.100.60" {
		t.Errorf("NatAddresses = %v", got.NatAddresses)
	}
	if got.Chassis != nil {
		t.Errorf("Chassis = %v, want nil for an unbound port", *got.Chassis)
	}
}

// TestKeyOfSBPortBindingIncludesSegmentColumns guards the segment-resolution
// inputs: a dropped UPDATE to datapath, tag, or options must register as
// content drift, or a stale cache could silently program the wrong VLAN.
func TestKeyOfSBPortBindingIncludesSegmentColumns(t *testing.T) {
	tag101, tag102 := 101, 102
	base := SBPortBinding{
		UUID: "pb-1", Type: "localnet", Datapath: "dp-1",
		Tag:     &tag101,
		Options: map[string]string{"network_name": "physnet1"},
	}

	variants := map[string]SBPortBinding{
		"datapath changed": func() SBPortBinding { v := base; v.Datapath = "dp-2"; return v }(),
		"tag changed":      func() SBPortBinding { v := base; v.Tag = &tag102; return v }(),
		"tag cleared":      func() SBPortBinding { v := base; v.Tag = nil; return v }(),
		"options changed": func() SBPortBinding {
			v := base
			v.Options = map[string]string{"network_name": "physnet2"}
			return v
		}(),
	}
	baseKey := keyOfSBPortBinding(base)
	for name, v := range variants {
		if keyOfSBPortBinding(v) == baseKey {
			t.Errorf("%s: content key did not change", name)
		}
	}
}

// TestKeyOfSBPortBindingIncludesExternalIDs guards the SB-NAT filter input:
// step 5b keys the router-gateway address off Port_Binding.external_ids
// (neutron:device_owner), so a dropped UPDATE to that map must register as
// content drift — otherwise the guard the check was built to provide would let
// a stale device_owner silently misdirect the desired set.
func TestKeyOfSBPortBindingIncludesExternalIDs(t *testing.T) {
	base := SBPortBinding{
		UUID: "pb-1", Type: "patch", LogicalPort: "external-port",
		ExternalIDs: map[string]string{"neutron:device_owner": "network:router_gateway"},
	}
	changed := base
	changed.ExternalIDs = map[string]string{"neutron:device_owner": "network:floatingip"}

	if keyOfSBPortBinding(changed) == keyOfSBPortBinding(base) {
		t.Error("external_ids change did not alter the content key")
	}
}

// TestDecodeSBPortBindingSegmentColumns round-trips the columns the segment
// resolution reads from a direct server select: the datapath UUID reference
// and the optional VLAN tag, including the OVSDB empty-set encoding for an
// absent tag.
func TestDecodeSBPortBindingSegmentColumns(t *testing.T) {
	row := ovsdb.Row{
		"_uuid":        ovsdb.UUID{GoUUID: "pb-ln"},
		"type":         "localnet",
		"logical_port": "seg-vlan",
		"datapath":     ovsdb.UUID{GoUUID: "dp-1"},
		// JSON-RPC delivers integers as float64.
		"tag":     float64(101),
		"options": ovsdb.OvsMap{GoMap: map[any]any{"network_name": "physnet1"}},
	}
	got := decodeSBPortBinding(row)
	if got.Datapath != "dp-1" {
		t.Errorf("Datapath = %q, want dp-1", got.Datapath)
	}
	if got.Tag == nil || *got.Tag != 101 {
		t.Errorf("Tag = %v, want 101", got.Tag)
	}

	// Absent tag arrives as an empty set and must decode to nil.
	flatRow := ovsdb.Row{
		"_uuid":    ovsdb.UUID{GoUUID: "pb-flat"},
		"type":     "localnet",
		"datapath": ovsdb.UUID{GoUUID: "dp-2"},
		"tag":      ovsdb.OvsSet{},
	}
	if got := decodeSBPortBinding(flatRow); got.Tag != nil {
		t.Errorf("flat Tag = %v, want nil", *got.Tag)
	}

	// A row without the columns at all decodes to zero values.
	if got := decodeSBPortBinding(ovsdb.Row{"_uuid": ovsdb.UUID{GoUUID: "pb-x"}}); got.Datapath != "" || got.Tag != nil {
		t.Errorf("empty row: Datapath=%q Tag=%v, want zero values", got.Datapath, got.Tag)
	}
}

func TestRowHelpers(t *testing.T) {
	t.Run("ovsSetToStrings", func(t *testing.T) {
		cases := []struct {
			name string
			in   any
			want []string
		}{
			{"bare string", "a", []string{"a"}},
			{"bare uuid", ovsdb.UUID{GoUUID: "u"}, []string{"u"}},
			{"set of strings", ovsdb.OvsSet{GoSet: []any{"a", "b"}}, []string{"a", "b"}},
			{"set of uuids", ovsdb.OvsSet{GoSet: []any{ovsdb.UUID{GoUUID: "u1"}}}, []string{"u1"}},
			{"empty set", ovsdb.OvsSet{GoSet: []any{}}, nil},
			{"absent", nil, nil},
		}
		for _, tc := range cases {
			if got := ovsSetToStrings(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: ovsSetToStrings(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
			}
		}
	})

	t.Run("rowOptString", func(t *testing.T) {
		if got := rowOptString(ovsdb.Row{"c": "x"}, "c"); got == nil || *got != "x" {
			t.Errorf("bare string: got %v", got)
		}
		if got := rowOptString(ovsdb.Row{"c": ovsdb.UUID{GoUUID: "u"}}, "c"); got == nil || *got != "u" {
			t.Errorf("bare uuid: got %v", got)
		}
		if got := rowOptString(ovsdb.Row{"c": ovsdb.OvsSet{GoSet: []any{}}}, "c"); got != nil {
			t.Errorf("empty set: got %v, want nil", *got)
		}
		if got := rowOptString(ovsdb.Row{}, "c"); got != nil {
			t.Errorf("absent: got %v, want nil", *got)
		}
	})
}

func TestKeySetsEqual(t *testing.T) {
	cases := []struct {
		name string
		a, b map[string]bool
		want bool
	}{
		{"equal", map[string]bool{"x": true, "y": true}, map[string]bool{"y": true, "x": true}, true},
		{"both empty", map[string]bool{}, map[string]bool{}, true},
		{"different len", map[string]bool{"x": true}, map[string]bool{"x": true, "y": true}, false},
		{"same len different member", map[string]bool{"x": true}, map[string]bool{"y": true}, false},
	}
	for _, tc := range cases {
		if got := keySetsEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: keySetsEqual = %v, want %v", tc.name, got, tc.want)
		}
	}
}
