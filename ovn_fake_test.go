package main

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/ovn-kubernetes/libovsdb/cache"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

// fakeOVSDBClient is a hand-rolled stand-in for the libovsdb client used by
// ovn_gateway.go. It serves List() from in-memory rows keyed by table name and
// records every transaction so tests can assert which OVSDB operations the
// production code emitted, without round-tripping through a real server.
//
// The fake intentionally does NOT apply mutations to its in-memory rows:
// each test sets the rows it wants the production code to see, then inspects
// transacts to verify the right ops were issued. This keeps the fake small
// while still exercising every write path in ovn_gateway.go.
type fakeOVSDBClient struct {
	dbm model.ClientDBModel

	mu        sync.Mutex
	rows      map[string][]model.Model
	transacts [][]ovsdb.Operation

	transactErr error
	listErr     error

	// opResults, when non-nil, is returned by Transact instead of the
	// default zero-result slice. Tests use this to simulate per-operation
	// OVSDB errors (e.g. constraint violations) that callers must detect
	// via ovsdb.CheckOperationResults.
	opResults []ovsdb.OperationResult

	// selectRows, when non-nil, supplies rows for OperationSelect ops keyed
	// by table name, bypassing the cache view exposed via setRows/List. The
	// drain fallback and refreshState's cache-consistency guard use
	// Transact(OperationSelect) to read directly from the server when the
	// cache appears incomplete or stale (issue #115); tests populate this to
	// simulate a "server has the row, cache does not" or "server has fresh
	// content, cache is stale" race. When a table has no entry here, Transact
	// synthesises a full row per cached model via modelToRow, i.e. the server
	// view is modelled as identical to the cache.
	selectRows map[string][]ovsdb.Row

	// onTransact is invoked synchronously for each Transact call (after the
	// op has been recorded). Used by drain tests to mutate rows mid-poll so
	// countLocalCRPorts can converge.
	onTransact func()

	// onList is invoked synchronously at the start of every List call,
	// outside the row lock so the hook may block other List operations.
	// Used by coalescing tests to hold a refresh in-flight.
	onList func()

	// connected backs Connected(); flipped by setConnected. Guarded by mu so
	// watchConnectionState's polling goroutine and the test goroutine do not
	// race under -race.
	connected bool
}

func newFakeOVSDBClient(dbm model.ClientDBModel) *fakeOVSDBClient {
	return &fakeOVSDBClient{
		dbm:  dbm,
		rows: make(map[string][]model.Model),
	}
}

// setRows replaces the in-memory rows for the given table. Accepts any
// pointer-to-model so call sites can pass concrete fixture types directly
// (e.g. *NBLogicalRouter) without boilerplate slice conversion.
func (f *fakeOVSDBClient) setRows(table string, rows ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(rows) == 0 {
		delete(f.rows, table)
		return
	}
	models := make([]model.Model, 0, len(rows))
	for _, r := range rows {
		models = append(models, r)
	}
	f.rows[table] = models
}

func (f *fakeOVSDBClient) recordedTransacts() [][]ovsdb.Operation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]ovsdb.Operation, len(f.transacts))
	for i, ops := range f.transacts {
		out[i] = append([]ovsdb.Operation(nil), ops...)
	}
	return out
}

// writeTransacts returns the recorded transactions that contain at least one
// non-select operation. cachedList issues read-only OperationSelect probes for
// its consistency check; tests asserting on writes filter those out.
func (f *fakeOVSDBClient) writeTransacts() [][]ovsdb.Operation {
	var out [][]ovsdb.Operation
	for _, batch := range f.recordedTransacts() {
		for _, op := range batch {
			if op.Op != ovsdb.OperationSelect {
				out = append(out, batch)
				break
			}
		}
	}
	return out
}

func (f *fakeOVSDBClient) tableForType(t reflect.Type) string {
	for table, mt := range f.dbm.Types() {
		if mt == t {
			return table
		}
	}
	return ""
}

// uuidOfModel reads the UUID field from a struct via reflection.
func uuidOfModel(m model.Model) string {
	v := reflect.ValueOf(m)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	f := v.FieldByName("UUID")
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}

// modelToRow renders a model.Model into the OVSDB wire-ish row shape the
// decode* helpers in ovn_cache.go consume, so a Transact(select) without an
// explicit setSelectRows override mirrors the cache exactly — the in-sync
// case the consistency guard must not flag as drift. Only the field kinds the
// production decoders read are emitted; anything else is left out of the row.
func modelToRow(m model.Model) ovsdb.Row {
	row := ovsdb.Row{}
	v := reflect.ValueOf(m)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("ovsdb")
		if tag == "" {
			continue
		}
		fv := v.Field(i)
		switch {
		case tag == "_uuid":
			row[tag] = ovsdb.UUID{GoUUID: fv.String()}
		case fv.Kind() == reflect.String:
			row[tag] = fv.String()
		case fv.Kind() == reflect.Int:
			row[tag] = int(fv.Int())
		case fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.String:
			// OVSDB encodes an absent optional as an empty set.
			if fv.IsNil() {
				row[tag] = ovsdb.OvsSet{}
			} else {
				row[tag] = fv.Elem().String()
			}
		case fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.Int:
			// Optional integer (e.g. Port_Binding.tag) — same set encoding.
			if fv.IsNil() {
				row[tag] = ovsdb.OvsSet{}
			} else {
				row[tag] = int(fv.Elem().Int())
			}
		case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.String:
			set := make([]any, fv.Len())
			for j := 0; j < fv.Len(); j++ {
				set[j] = fv.Index(j).String()
			}
			row[tag] = ovsdb.OvsSet{GoSet: set}
		case fv.Kind() == reflect.Map:
			gm := make(map[any]any, fv.Len())
			for iter := fv.MapRange(); iter.Next(); {
				gm[iter.Key().Interface()] = iter.Value().Interface()
			}
			row[tag] = ovsdb.OvsMap{GoMap: gm}
		}
	}
	return row
}

// --- ovsdbClient surface -----------------------------------------------------

func (f *fakeOVSDBClient) Connect(context.Context) error { return nil }
func (f *fakeOVSDBClient) Close()                        {}
func (f *fakeOVSDBClient) Cache() *cache.TableCache      { return nil }

// Connected reports the fake's simulated connection status. setConnected drives
// it so tests can exercise watchConnectionState's drop/recover mirroring.
func (f *fakeOVSDBClient) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeOVSDBClient) setConnected(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = v
}

func (f *fakeOVSDBClient) NewMonitor(_ ...client.MonitorOption) *client.Monitor {
	return &client.Monitor{}
}

func (f *fakeOVSDBClient) Monitor(context.Context, *client.Monitor) (client.MonitorCookie, error) {
	return client.MonitorCookie{}, nil
}

func (f *fakeOVSDBClient) List(_ context.Context, result interface{}) error {
	if f.listErr != nil {
		return f.listErr
	}
	f.mu.Lock()
	hook := f.onList
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	out := reflect.ValueOf(result).Elem()
	if out.Kind() != reflect.Slice {
		return fmt.Errorf("fakeOVSDBClient.List: result must be pointer to slice, got %T", result)
	}
	elemType := out.Type().Elem()
	ptrType := reflect.PointerTo(elemType)

	table := f.tableForType(ptrType)
	if table == "" {
		return fmt.Errorf("fakeOVSDBClient.List: no table mapping for %s", elemType)
	}

	rows := f.rows[table]
	slice := reflect.MakeSlice(out.Type(), len(rows), len(rows))
	for i, r := range rows {
		slice.Index(i).Set(reflect.ValueOf(r).Elem())
	}
	out.Set(slice)
	return nil
}

func (f *fakeOVSDBClient) Create(models ...model.Model) ([]ovsdb.Operation, error) {
	ops := make([]ovsdb.Operation, 0, len(models))
	for _, m := range models {
		ops = append(ops, ovsdb.Operation{
			Op:       ovsdb.OperationInsert,
			Table:    f.tableForType(reflect.TypeOf(m)),
			UUIDName: uuidOfModel(m),
		})
	}
	return ops, nil
}

func (f *fakeOVSDBClient) Where(models ...model.Model) client.ConditionalAPI {
	var target model.Model
	if len(models) > 0 {
		target = models[0]
	}
	return &fakeConditionalAPI{client: f, target: target}
}

func (f *fakeOVSDBClient) Transact(_ context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error) {
	f.mu.Lock()
	f.transacts = append(f.transacts, append([]ovsdb.Operation(nil), ops...))
	hook := f.onTransact
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	if f.transactErr != nil {
		return nil, f.transactErr
	}
	if f.opResults != nil {
		return f.opResults, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	results := make([]ovsdb.OperationResult, len(ops))
	for i, op := range ops {
		if op.Op != ovsdb.OperationSelect {
			continue
		}
		if rows, ok := f.selectRows[op.Table]; ok {
			results[i].Rows = append([]ovsdb.Row(nil), rows...)
			continue
		}
		// No explicit override: model the server view as identical to the
		// cache. The consistency check builds a content key over several
		// columns, so the synthesised row must mirror the whole model — a
		// bare _uuid row would read back as drift on every refresh.
		for _, m := range f.rows[op.Table] {
			results[i].Rows = append(results[i].Rows, modelToRow(m))
		}
	}
	return results, nil
}

// setSelectRows registers rows returned by Transact(OperationSelect) for the
// given table. The rows reflect the server-side view, which may differ from
// the cache view set via setRows.
func (f *fakeOVSDBClient) setSelectRows(table string, rows ...ovsdb.Row) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.selectRows == nil {
		f.selectRows = make(map[string][]ovsdb.Row)
	}
	if len(rows) == 0 {
		delete(f.selectRows, table)
		return
	}
	f.selectRows[table] = append([]ovsdb.Row(nil), rows...)
}

// --- ConditionalAPI surface --------------------------------------------------

type fakeConditionalAPI struct {
	client *fakeOVSDBClient
	target model.Model
}

func (c *fakeConditionalAPI) List(context.Context, any) error { return nil }

func (c *fakeConditionalAPI) Mutate(model.Model, ...model.Mutation) ([]ovsdb.Operation, error) {
	return nil, nil
}

// Update returns a synthetic op whose Row maps each selected field's
// ovsdb column tag to the field's current value, so tests can inspect
// what the production code is about to write (e.g. a boosted priority).
// Field pointers are matched to struct fields by address; if no fields
// are passed, all ovsdb-tagged fields are included.
func (c *fakeConditionalAPI) Update(m model.Model, fields ...any) ([]ovsdb.Operation, error) {
	row := ovsdb.Row{}
	v := reflect.ValueOf(m)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("fakeConditionalAPI.Update: expected pointer to struct, got %T", m)
	}
	if len(fields) == 0 {
		for i := 0; i < v.NumField(); i++ {
			tag := v.Type().Field(i).Tag.Get("ovsdb")
			if tag == "" || tag == "_uuid" {
				continue
			}
			row[tag] = v.Field(i).Interface()
		}
	} else {
		for _, fp := range fields {
			fpv := reflect.ValueOf(fp)
			if fpv.Kind() != reflect.Pointer {
				continue
			}
			fpAddr := fpv.Pointer()
			for i := 0; i < v.NumField(); i++ {
				if v.Field(i).CanAddr() && v.Field(i).Addr().Pointer() == fpAddr {
					tag := v.Type().Field(i).Tag.Get("ovsdb")
					if tag != "" && tag != "_uuid" {
						row[tag] = v.Field(i).Interface()
					}
					break
				}
			}
		}
	}
	return []ovsdb.Operation{{
		Op:    ovsdb.OperationUpdate,
		Table: c.client.tableForType(reflect.TypeOf(m)),
		UUID:  uuidOfModel(m),
		Row:   row,
	}}, nil
}

func (c *fakeConditionalAPI) Delete() ([]ovsdb.Operation, error) {
	if c.target == nil {
		return nil, nil
	}
	return []ovsdb.Operation{{
		Op:    ovsdb.OperationDelete,
		Table: c.client.tableForType(reflect.TypeOf(c.target)),
		UUID:  uuidOfModel(c.target),
	}}, nil
}

func (c *fakeConditionalAPI) Wait(ovsdb.WaitCondition, *int, model.Model, ...any) ([]ovsdb.Operation, error) {
	return nil, nil
}

// Compile-time assertions.
var (
	_ ovsdbClient           = (*fakeOVSDBClient)(nil)
	_ client.ConditionalAPI = (*fakeConditionalAPI)(nil)
)

// newOVNClientWithFakes returns an OVNClient wired to two fakeOVSDBClient
// instances built from the production NB/SB schemas. The returned OVNClient
// has its LocalChassisName preset for tests that read it.
func newOVNClientWithFakes(t testingT, localChassis string) (*OVNClient, *fakeOVSDBClient, *fakeOVSDBClient) {
	t.Helper()
	nbm, err := nbDatabaseModel()
	if err != nil {
		t.Fatalf("nbDatabaseModel: %v", err)
	}
	sbm, err := sbDatabaseModel()
	if err != nil {
		t.Fatalf("sbDatabaseModel: %v", err)
	}
	nb := newFakeOVSDBClient(nbm)
	sb := newFakeOVSDBClient(sbm)
	c := NewOVNClient(Config{}, nil)
	c.nbClient = nb
	c.sbClient = sb
	c.state.LocalChassisName = localChassis
	return c, nb, sb
}

// testingT is a tiny shim so the helper can be used from both *testing.T and
// *testing.B without importing testing here.
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}
