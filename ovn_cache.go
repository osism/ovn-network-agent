package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// =============================================================================
// Monitor-cache consistency guard
// =============================================================================
//
// refreshState builds the agent's entire view of FIPs and locally-active
// routers from the libovsdb monitor cache. That cache can drift from the OVN
// server in two ways:
//
//   - A dropped table INSERT delivered shortly after a fresh monitor
//     subscription (issue #115): the row is missing from the cache entirely.
//   - A dropped UPDATE to a row already in the cache: the row is present but
//     a column value is stale.
//
// Both are silently dangerous, and the HA-failover signal lives entirely in
// reference/set columns — Port_Binding.chassis (which chassis owns the
// chassisredirect port), Logical_Router.nat/.ports (which FIPs belong to a
// router), Logical_Router_Port.networks. A missing row or a stale column
// shrinks or misdirects the desired-IP set, and ensureRoutes then withdraws
// the FRR /32 of every FIP it no longer sees as desired — the FIP stops being
// advertised even though OVN itself is perfectly healthy, so the node looks
// fine but is unreachable from outside.
//
// The cache stays the primary, fast path. On every refresh the cache snapshot
// is cross-checked against a server-side select of the failover-critical
// columns: a content key is built per row from exactly those columns, so a
// stale column value — not only a dropped or extra row — registers as a
// mismatch. A bare _uuid comparison cannot see a dropped UPDATE because the
// _uuid set is unchanged. Only on a genuine mismatch is the slice rebuilt from
// a full direct select; any error reading from the server degrades gracefully
// to the cache so a transient OVSDB blip cannot stall reconciliation.

// The *CheckColumns slices list, per table, the columns whose drift would
// change the desired-IP set refreshState computes. The consistency check
// selects exactly these — keeping the common in-sync round-trip cheap — and
// the matching keyOf* helper must read only the model fields they back.
var (
	pbCheckColumns   = []string{"_uuid", "type", "chassis", "nat_addresses", "datapath", "tag", "options"}
	chCheckColumns   = []string{"_uuid", "hostname"}
	lrCheckColumns   = []string{"_uuid", "ports", "nat", "static_routes"}
	lrpCheckColumns  = []string{"_uuid", "mac", "networks"}
	natCheckColumns  = []string{"_uuid", "type", "external_ip"}
	lrsrCheckColumns = []string{"_uuid", "ip_prefix", "nexthop", "output_port", "external_ids"}
	smbCheckColumns  = []string{"_uuid", "logical_port", "ip", "mac"}
)

func keyOfSBPortBinding(p SBPortBinding) string {
	chassis := ""
	if p.Chassis != nil {
		chassis = *p.Chassis
	}
	tag := ""
	if p.Tag != nil {
		tag = strconv.Itoa(*p.Tag)
	}
	// fmt %v renders a map with its keys sorted, so the key stays stable.
	return contentKey(p.UUID, p.Type, chassis, sortedJoin(p.NatAddresses),
		p.Datapath, tag, fmt.Sprintf("%v", p.Options))
}

func keyOfSBChassis(c SBChassis) string {
	return contentKey(c.UUID, c.Hostname)
}

func keyOfNBLogicalRouter(r NBLogicalRouter) string {
	return contentKey(r.UUID, sortedJoin(r.Ports), sortedJoin(r.Nat), sortedJoin(r.StaticRoutes))
}

func keyOfNBLogicalRouterPort(p NBLogicalRouterPort) string {
	return contentKey(p.UUID, p.MAC, sortedJoin(p.Networks))
}

func keyOfNBNAT(n NBNAT) string {
	return contentKey(n.UUID, n.Type, n.ExternalIP)
}

func keyOfNBLogicalRouterStaticRoute(r NBLogicalRouterStaticRoute) string {
	outputPort := ""
	if r.OutputPort != nil {
		outputPort = *r.OutputPort
	}
	// fmt %v renders a map with its keys sorted, so the key stays stable.
	return contentKey(r.UUID, r.IPPrefix, r.Nexthop, outputPort, fmt.Sprintf("%v", r.ExternalIDs))
}

func keyOfNBStaticMACBinding(b NBStaticMACBinding) string {
	return contentKey(b.UUID, b.LogicalPort, b.IP, b.MAC)
}

// contentKey joins row attributes into a single comparison key. The unit
// separator (0x1f) cannot occur in a UUID, MAC, IP, hostname or OVN name, so
// distinct attribute tuples always yield distinct keys.
func contentKey(parts ...string) string {
	return strings.Join(parts, "\x1f")
}

// sortedJoin renders a set-valued column deterministically. OVSDB sets are
// unordered, so the monitor cache and a direct select may list the members in
// different orders; that must not register as content drift.
func sortedJoin(values []string) string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// cachedList lists a table from the monitor cache and then cross-checks it
// against the OVN server. It returns a server-authoritative slice whenever the
// monitor cache turns out to be incomplete or to hold a stale column value.
// columns is the cheap server-side check projection; keyOf builds a content
// key over exactly those columns; decode rebuilds a model from a raw row.
func cachedList[T any](
	ctx context.Context,
	c ovsdbClient,
	table string,
	columns []string,
	keyOf func(T) string,
	decode func(ovsdb.Row) T,
) ([]T, error) {
	var cached []T
	if err := c.List(ctx, &cached); err != nil {
		return nil, err
	}
	return authoritativeList(ctx, c, table, columns, cached, keyOf, decode), nil
}

// authoritativeList returns the rows of table that refreshState should act on.
// It trusts the monitor-cache snapshot (cached) unless a direct server select
// of the failover-critical columns proves it stale, in which case it logs the
// gap and rebuilds the slice from a full select. Any error reading from the
// server degrades gracefully to the cache: a transient OVSDB blip must not
// stall reconciliation.
func authoritativeList[T any](
	ctx context.Context,
	c ovsdbClient,
	table string,
	columns []string,
	cached []T,
	keyOf func(T) string,
	decode func(ovsdb.Row) T,
) []T {
	serverKeys, err := serverKeySet(ctx, c, table, columns, keyOf, decode)
	if err != nil {
		slog.Warn("cache consistency check skipped: direct select failed, trusting monitor cache",
			"table", table, "error", err)
		return cached
	}

	cacheKeys := make(map[string]bool, len(cached))
	for _, m := range cached {
		cacheKeys[keyOf(m)] = true
	}
	if keySetsEqual(cacheKeys, serverKeys) {
		return cached
	}

	slog.Error("OVN monitor cache out of sync with the server, rebuilding from a direct select",
		"table", table,
		"cache_rows", len(cacheKeys),
		"server_rows", len(serverKeys))

	rows, err := directSelect(ctx, c, table, nil)
	if err != nil {
		slog.Error("cache recovery select failed, falling back to the monitor cache",
			"table", table, "error", err)
		return cached
	}
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		out = append(out, decode(row))
	}
	return out
}

// serverKeySet returns the set of content keys currently in table on the OVN
// server. Only the failover-critical columns are selected so the common
// (in-sync) path stays cheap; keyOf must build its key from no more than those.
func serverKeySet[T any](
	ctx context.Context,
	c ovsdbClient,
	table string,
	columns []string,
	keyOf func(T) string,
	decode func(ovsdb.Row) T,
) (map[string]bool, error) {
	rows, err := directSelect(ctx, c, table, columns)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(rows))
	for _, row := range rows {
		set[keyOf(decode(row))] = true
	}
	return set, nil
}

// directSelect runs an OVSDB select on table, bypassing the monitor cache.
// With columns nil every column is returned; otherwise only the named ones.
// Where is left empty — Operation.MarshalJSON encodes a conditionless select
// as the match-everything form.
func directSelect(ctx context.Context, c ovsdbClient, table string, columns []string) ([]ovsdb.Row, error) {
	op := ovsdb.Operation{
		Op:      ovsdb.OperationSelect,
		Table:   table,
		Columns: columns,
	}
	results, err := c.Transact(ctx, op)
	if err != nil {
		return nil, fmt.Errorf("select from %s: %w", table, err)
	}
	if _, err := ovsdb.CheckOperationResults(results, []ovsdb.Operation{op}); err != nil {
		return nil, fmt.Errorf("select from %s: %w", table, err)
	}
	if len(results) == 0 {
		return nil, nil
	}
	return results[0].Rows, nil
}

// keySetsEqual reports whether two content-key sets contain exactly the same
// members.
func keySetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// =============================================================================
// Raw OVSDB row decoders
// =============================================================================
//
// These rebuild the typed models from the raw rows returned by directSelect.
// Only the columns refreshState actually consumes are decoded; everything else
// is left at its zero value. OVSDB encodes a set of exactly one element as the
// bare element and larger sets as an OvsSet, so the helpers normalise both.

func decodeSBPortBinding(row ovsdb.Row) SBPortBinding {
	return SBPortBinding{
		UUID:         rowUUID(row),
		Type:         rowString(row, "type"),
		LogicalPort:  rowString(row, "logical_port"),
		Chassis:      rowOptString(row, "chassis"),
		Datapath:     rowUUIDRef(row, "datapath"),
		Tag:          rowOptInt(row, "tag"),
		Options:      rowStringMap(row, "options"),
		ExternalIDs:  rowStringMap(row, "external_ids"),
		NatAddresses: rowStringSet(row, "nat_addresses"),
	}
}

func decodeSBChassis(row ovsdb.Row) SBChassis {
	return SBChassis{
		UUID:     rowUUID(row),
		Name:     rowString(row, "name"),
		Hostname: rowString(row, "hostname"),
	}
}

func decodeNBNAT(row ovsdb.Row) NBNAT {
	return NBNAT{
		UUID:       rowUUID(row),
		Type:       rowString(row, "type"),
		ExternalIP: rowString(row, "external_ip"),
	}
}

func decodeNBLogicalRouter(row ovsdb.Row) NBLogicalRouter {
	return NBLogicalRouter{
		UUID:         rowUUID(row),
		Name:         rowString(row, "name"),
		Ports:        rowStringSet(row, "ports"),
		Nat:          rowStringSet(row, "nat"),
		StaticRoutes: rowStringSet(row, "static_routes"),
	}
}

func decodeNBLogicalRouterPort(row ovsdb.Row) NBLogicalRouterPort {
	return NBLogicalRouterPort{
		UUID:     rowUUID(row),
		Name:     rowString(row, "name"),
		MAC:      rowString(row, "mac"),
		Networks: rowStringSet(row, "networks"),
	}
}

func decodeNBLogicalRouterStaticRoute(row ovsdb.Row) NBLogicalRouterStaticRoute {
	return NBLogicalRouterStaticRoute{
		UUID:        rowUUID(row),
		IPPrefix:    rowString(row, "ip_prefix"),
		Nexthop:     rowString(row, "nexthop"),
		OutputPort:  rowOptString(row, "output_port"),
		ExternalIDs: rowStringMap(row, "external_ids"),
	}
}

func decodeNBStaticMACBinding(row ovsdb.Row) NBStaticMACBinding {
	return NBStaticMACBinding{
		UUID:        rowUUID(row),
		LogicalPort: rowString(row, "logical_port"),
		IP:          rowString(row, "ip"),
		MAC:         rowString(row, "mac"),
	}
}

// rowUUID extracts the _uuid column from a raw OVSDB row.
func rowUUID(row ovsdb.Row) string {
	if u, ok := row["_uuid"].(ovsdb.UUID); ok {
		return u.GoUUID
	}
	return ""
}

// rowString returns a scalar string column, or "" when absent.
func rowString(row ovsdb.Row, col string) string {
	s, _ := row[col].(string)
	return s
}

// rowOptString returns an optional (set-of-max-1) column as a pointer. OVSDB
// sends an empty optional as an empty set and a present one as the bare value;
// UUID references are flattened to their GoUUID string.
func rowOptString(row ovsdb.Row, col string) *string {
	switch v := row[col].(type) {
	case string:
		return &v
	case ovsdb.UUID:
		s := v.GoUUID
		return &s
	case ovsdb.OvsSet:
		if vals := ovsSetToStrings(v); len(vals) > 0 {
			return &vals[0]
		}
	}
	return nil
}

// rowUUIDRef returns a strong UUID-reference column flattened to its GoUUID
// string, or "" when absent. The monitor cache stores such columns as plain
// strings, so both encodings are accepted.
func rowUUIDRef(row ovsdb.Row, col string) string {
	switch v := row[col].(type) {
	case ovsdb.UUID:
		return v.GoUUID
	case string:
		return v
	}
	return ""
}

// rowOptInt returns an optional integer column as a pointer. OVSDB sends an
// absent optional as an empty set and a present one as the bare number;
// JSON-RPC delivers integers as float64, while an in-process fake may pass int.
func rowOptInt(row ovsdb.Row, col string) *int {
	v := row[col]
	if s, ok := v.(ovsdb.OvsSet); ok {
		if len(s.GoSet) == 0 {
			return nil
		}
		v = s.GoSet[0]
	}
	switch n := v.(type) {
	case int:
		i := n
		return &i
	case float64:
		i := int(n)
		return &i
	}
	return nil
}

// rowStringSet returns a set-valued column as a Go slice.
func rowStringSet(row ovsdb.Row, col string) []string {
	return ovsSetToStrings(row[col])
}

// ovsSetToStrings flattens an OVSDB set value into a string slice, handling the
// bare-single-element encoding and flattening UUID references to GoUUID.
func ovsSetToStrings(v any) []string {
	switch s := v.(type) {
	case string:
		return []string{s}
	case ovsdb.UUID:
		return []string{s.GoUUID}
	case ovsdb.OvsSet:
		out := make([]string, 0, len(s.GoSet))
		for _, e := range s.GoSet {
			switch ev := e.(type) {
			case string:
				out = append(out, ev)
			case ovsdb.UUID:
				out = append(out, ev.GoUUID)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// rowStringMap returns a string→string map column, or nil when absent.
func rowStringMap(row ovsdb.Row, col string) map[string]string {
	m, ok := row[col].(ovsdb.OvsMap)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m.GoMap))
	for k, v := range m.GoMap {
		ks, kok := k.(string)
		vs, vok := v.(string)
		if kok && vok {
			out[ks] = vs
		}
	}
	return out
}
