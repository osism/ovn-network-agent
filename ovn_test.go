package main

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/model"
)

func TestOvsdbEndpoints(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   []string
	}{
		{"single unix", "unix:/var/run/ovn/ovnsb_db.sock", []string{"unix:/var/run/ovn/ovnsb_db.sock"}},
		{"single tcp", "tcp:10.0.0.1:6642", []string{"tcp:10.0.0.1:6642"}},
		{"multiple endpoints", "tcp:10.0.0.1:6642,tcp:10.0.0.2:6642", []string{"tcp:10.0.0.1:6642", "tcp:10.0.0.2:6642"}},
		{"with whitespace", " tcp:10.0.0.1:6642 , tcp:10.0.0.2:6642 ", []string{"tcp:10.0.0.1:6642", "tcp:10.0.0.2:6642"}},
		{"trailing comma", "tcp:10.0.0.1:6642,", []string{"tcp:10.0.0.1:6642"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ovsdbEndpoints(tt.remote)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ovsdbEndpoints(%q) = %v, want %v", tt.remote, got, tt.want)
			}
		})
	}
}

func TestGetHostname(t *testing.T) {
	h, err := getHostname()
	if err != nil {
		t.Fatalf("getHostname() error: %v", err)
	}
	if h == "" {
		t.Error("getHostname() returned empty string")
	}
	// Should not contain dots (FQDN stripped).
	for _, c := range h {
		if c == '.' {
			t.Errorf("getHostname() = %q, should not contain dots", h)
			break
		}
	}
}

func TestNewOVNClient(t *testing.T) {
	called := false
	cb := func() { called = true }

	cfg := Config{
		OVNSBRemote: "unix:/tmp/sb.sock",
		OVNNBRemote: "unix:/tmp/nb.sock",
	}

	c := NewOVNClient(cfg, cb)

	// The state holder's zero value is usable: a snapshot taken before any
	// refresh yields an empty state (with initialised maps) rather than
	// panicking or exposing nil collections.
	snap := c.GetState()
	if snap.HasLocalRouters || len(snap.LocalRouters) != 0 {
		t.Errorf("fresh client state = %+v, want empty", snap)
	}
	if snap.NATIPToRouterMAC == nil || snap.AllChassisNames == nil {
		t.Error("snapshot maps should be initialised, not nil")
	}
	if c.cfg.OVNSBRemote != cfg.OVNSBRemote {
		t.Errorf("cfg.OVNSBRemote = %q, want %q", c.cfg.OVNSBRemote, cfg.OVNSBRemote)
	}

	c.onChange()
	if !called {
		t.Error("onChange callback was not invoked")
	}
}

func TestGetStateSnapshot(t *testing.T) {
	c := NewOVNClient(Config{}, nil)
	c.state.Replace(OVNState{
		LocalChassisName: "node1",
		LocalRouters: []LocalRouterInfo{
			{RouterName: "router1", RouterUUID: "uuid1", LRPName: "lrp-abc", LRPUUID: "lrp-uuid1", LRPNetworks: []string{"10.0.0.1/24"}, CRPort: "cr-lrp-abc"},
			{RouterName: "router2", RouterUUID: "uuid2", LRPName: "lrp-def", LRPUUID: "lrp-uuid2", LRPNetworks: []string{"172.16.0.1/16"}, CRPort: "cr-lrp-def"},
		},
		HasLocalRouters: true,
		SNATIPs:         []string{"10.0.0.100", "10.0.0.101"},
	})

	snap := c.GetState()

	if snap.LocalChassisName != "node1" {
		t.Errorf("LocalChassisName = %q, want %q", snap.LocalChassisName, "node1")
	}
	if !snap.HasLocalRouters {
		t.Error("HasLocalRouters should be true")
	}
	if len(snap.LocalRouters) != 2 {
		t.Errorf("LocalRouters length = %d, want 2", len(snap.LocalRouters))
	}
	if snap.LocalRouters[0].RouterName != "router1" {
		t.Errorf("LocalRouters[0].RouterName = %q, want %q", snap.LocalRouters[0].RouterName, "router1")
	}
	if len(snap.SNATIPs) != 2 {
		t.Errorf("SNATIPs length = %d, want 2", len(snap.SNATIPs))
	}

	// Verify snapshot is a copy: mutating it must not affect a fresh snapshot
	// read back from the holder's live state.
	snap.SNATIPs[0] = "modified"
	if c.GetState().SNATIPs[0] == "modified" {
		t.Error("GetState should return a copy of SNATIPs, not a reference")
	}

	snap.LocalRouters[0].RouterName = "modified"
	if c.GetState().LocalRouters[0].RouterName == "modified" {
		t.Error("GetState should return a copy of LocalRouters, not a reference")
	}
}

// TestOVNStateCloneReallocatesEveryReferenceField is the guard that replaces
// the old hand-maintained 9-field deep copy: it walks OVNState by reflection
// and asserts that clone() re-allocates every reference-typed field (slice,
// map, pointer). Adding such a field to OVNState without extending clone()
// fails here — where the old code would have silently handed out a snapshot
// aliasing (or zero-valuing) the holder's live state.
func TestOVNStateCloneReallocatesEveryReferenceField(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("198.51.100.0/24")
	// Populate every field so each reference type has something to alias.
	src := OVNState{
		LocalChassisName:   "node1",
		LocalRouters:       []LocalRouterInfo{{RouterName: "r1"}},
		HasLocalRouters:    true,
		SNATIPs:            []string{"198.51.100.1"},
		NATIPToRouterMAC:   map[string]string{"198.51.100.1": "aa:bb:cc:dd:ee:ff"},
		NATIPToSegment:     map[string]string{"198.51.100.1": "physnet1"},
		DiscoveredNetworks: []*net.IPNet{cidr},
		AllChassisNames:    map[string]bool{"node1": true},
	}

	// Every field must be non-zero, or the re-allocation check below would
	// vacuously pass for a field the fixture forgot to populate.
	sv := reflect.ValueOf(src)
	for i := 0; i < sv.NumField(); i++ {
		if sv.Field(i).IsZero() {
			t.Fatalf("fixture does not populate OVNState.%s — extend it, then extend clone()",
				sv.Type().Field(i).Name)
		}
	}

	got := reflect.ValueOf(src.clone())
	for i := 0; i < sv.NumField(); i++ {
		name := sv.Type().Field(i).Name
		orig, cloned := sv.Field(i), got.Field(i)
		switch orig.Kind() {
		case reflect.Slice, reflect.Map:
			if orig.UnsafePointer() == cloned.UnsafePointer() {
				t.Errorf("clone() did not re-allocate OVNState.%s — the snapshot aliases the live state", name)
			}
			if orig.Len() != cloned.Len() {
				t.Errorf("clone() lost content of OVNState.%s: len %d, want %d", name, cloned.Len(), orig.Len())
			}
		default:
			// Scalars copy by value; just verify the content survived.
			if !reflect.DeepEqual(orig.Interface(), cloned.Interface()) {
				t.Errorf("clone() lost OVNState.%s: got %v, want %v", name, cloned, orig)
			}
		}
	}
}

func TestGetStateSnapshotNoLocalRouters(t *testing.T) {
	c := NewOVNClient(Config{}, nil)
	c.state.SetLocalChassisName("node1")

	snap := c.GetState()

	if snap.HasLocalRouters {
		t.Error("HasLocalRouters should be false when no routers are set")
	}
	if len(snap.LocalRouters) != 0 {
		t.Errorf("LocalRouters length = %d, want 0", len(snap.LocalRouters))
	}
}

func TestUniqueNetworks(t *testing.T) {
	_, net1, _ := net.ParseCIDR("10.0.0.0/24")
	_, net2, _ := net.ParseCIDR("172.16.0.0/16")
	_, net1dup, _ := net.ParseCIDR("10.0.0.0/24")

	tests := []struct {
		name  string
		input []*net.IPNet
		want  []string
	}{
		{"nil", nil, []string{}},
		{"empty", []*net.IPNet{}, []string{}},
		{"single", []*net.IPNet{net1}, []string{"10.0.0.0/24"}},
		{"dedup", []*net.IPNet{net1, net2, net1dup}, []string{"10.0.0.0/24", "172.16.0.0/16"}},
		{"sorted", []*net.IPNet{net2, net1}, []string{"10.0.0.0/24", "172.16.0.0/16"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := uniqueNetworks(tt.input)
			gotStrs := make([]string, 0, len(got))
			for _, n := range got {
				gotStrs = append(gotStrs, n.String())
			}
			if !reflect.DeepEqual(gotStrs, tt.want) {
				t.Errorf("uniqueNetworks() = %v, want %v", gotStrs, tt.want)
			}
		})
	}
}

func TestGetStateIncludesDiscoveredNetworks(t *testing.T) {
	_, net1, _ := net.ParseCIDR("198.51.100.0/24")
	c := NewOVNClient(Config{}, nil)
	c.state.Replace(OVNState{DiscoveredNetworks: []*net.IPNet{net1}})

	snap := c.GetState()
	if len(snap.DiscoveredNetworks) != 1 {
		t.Fatalf("DiscoveredNetworks length = %d, want 1", len(snap.DiscoveredNetworks))
	}
	if snap.DiscoveredNetworks[0].String() != "198.51.100.0/24" {
		t.Errorf("DiscoveredNetworks[0] = %q, want %q", snap.DiscoveredNetworks[0], "198.51.100.0/24")
	}

	// Verify snapshot is a copy.
	snap.DiscoveredNetworks[0] = nil
	if c.GetState().DiscoveredNetworks[0] == nil {
		t.Error("GetState should return a copy of DiscoveredNetworks")
	}
}

func TestGetStateIncludesAllChassisNames(t *testing.T) {
	c := NewOVNClient(Config{}, nil)
	c.state.Replace(OVNState{AllChassisNames: map[string]bool{
		"node-1": true,
		"node-2": true,
	}})

	snap := c.GetState()
	if len(snap.AllChassisNames) != 2 {
		t.Fatalf("AllChassisNames length = %d, want 2", len(snap.AllChassisNames))
	}
	if !snap.AllChassisNames["node-1"] || !snap.AllChassisNames["node-2"] {
		t.Errorf("AllChassisNames = %v, want node-1 and node-2", snap.AllChassisNames)
	}

	// Verify snapshot is a copy.
	snap.AllChassisNames["node-3"] = true
	if c.GetState().AllChassisNames["node-3"] {
		t.Error("GetState should return a copy of AllChassisNames")
	}
}

// TestWaitRefreshLoopStoppedNilSafe covers the port-forward-only / never-
// connected path: with loopDone unset the wait must return immediately rather
// than block the shutdown sequence forever.
func TestWaitRefreshLoopStoppedNilSafe(t *testing.T) {
	c := NewOVNClient(Config{}, nil)
	done := make(chan struct{})
	go func() {
		c.waitRefreshLoopStopped()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitRefreshLoopStopped blocked with a nil loopDone")
	}
}

// TestWaitRefreshLoopStoppedWaitsForInFlight pins the shutdown-ordering
// contract: the wait must not return until the refresh loop has fully exited,
// modelling a loop still finishing one in-flight refreshState after ctx was
// cancelled. If it returned early the shutdown path's post-drain refresh would
// interleave with that final loop refresh.
func TestWaitRefreshLoopStoppedWaitsForInFlight(t *testing.T) {
	c := NewOVNClient(Config{}, nil)
	c.loopDone = make(chan struct{})

	var loopExited atomic.Bool
	go func() {
		// Model the in-flight refresh completing before the loop returns.
		time.Sleep(20 * time.Millisecond)
		loopExited.Store(true)
		close(c.loopDone)
	}()

	c.waitRefreshLoopStopped()
	if !loopExited.Load() {
		t.Fatal("waitRefreshLoopStopped returned before the refresh loop exited")
	}
}

func TestHostnamesEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical short", "host-a", "host-a", true},
		{"fqdn vs short", "host-a.example.com", "host-a", true},
		{"both fqdn", "host-a.example.com", "host-a.internal", true},
		{"case insensitive label", "HOST-A.example.com", "host-a", true},
		{"different host", "host-a.example.com", "host-b", false},
		{"different host both fqdn", "host-a.example.com", "host-b.example.com", false},
		{"empty vs short", "", "host-a", false},
		{"both empty", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostnamesEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("hostnamesEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestRefreshStateMatchesFQDNChassisHostname pins the domain-strip fix: SB
// Chassis.hostname is an FQDN while the agent's local chassis name is the
// short form. An exact-match comparison would treat the local router as
// remote and drop it; the shortHostname join must recognise it as local and
// normalise the AllChassisNames set to the short label cleanupStaleChassis
// expects.
func TestRefreshStateMatchesFQDNChassisHostname(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setRows("Chassis",
		&SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a.example.com"},
	)
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
		Ports: []string{"lrp-uuid-local"}, Nat: []string{"nat-fip"},
	})
	nb.setRows("NAT", &NBNAT{UUID: "nat-fip", Type: "dnat_and_snat", ExternalIP: "198.51.100.50"})

	c.state.SetLocalChassisName("host-a")
	c.refreshState(context.Background())
	snap := c.GetState()

	if !snap.HasLocalRouters {
		t.Fatal("HasLocalRouters = false — FQDN chassis hostname not matched to short local name")
	}
	if !snap.AllChassisNames["host-a"] {
		t.Errorf("AllChassisNames = %v, want normalized short key host-a", snap.AllChassisNames)
	}
}

func TestParseNatAddressIPs(t *testing.T) {
	tests := []struct {
		name    string
		natAddr string
		want    []string
	}{
		{
			"typical SNAT with chassis_resident",
			`fa:16:3e:8f:45:69 198.51.100.15 is_chassis_resident("cr-lrp-d8bba1ed-55eb-4476-9c3e-bedc07388cac")`,
			[]string{"198.51.100.15"},
		},
		{
			"multiple IPs",
			"fa:16:3e:ab:cd:ef 10.0.0.1 10.0.0.2 is_chassis_resident(\"cr-lrp-abc\")",
			[]string{"10.0.0.1", "10.0.0.2"},
		},
		{
			"MAC and IP only",
			"fa:16:3e:ab:cd:ef 192.168.1.1",
			[]string{"192.168.1.1"},
		},
		{
			"MAC only",
			"fa:16:3e:ab:cd:ef",
			nil,
		},
		{
			"empty string",
			"",
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseNatAddressIPs(tt.natAddr)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseNatAddressIPs(%q) = %v, want %v", tt.natAddr, got, tt.want)
			}
		})
	}
}

// closeCountingClient is a minimal ovsdbClient that counts Close() calls.
// Used to verify that Connect()'s failure path actually closes partial
// clients instead of leaking them across retries (issue #26).
type closeCountingClient struct {
	*fakeOVSDBClient
	closes int
}

func (c *closeCountingClient) Close() { c.closes++ }

func TestCloseClients_ClosesAndClearsBoth(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	sbCounter := &closeCountingClient{fakeOVSDBClient: sb}
	nbCounter := &closeCountingClient{fakeOVSDBClient: nb}
	c.sbClient = sbCounter
	c.nbClient = nbCounter

	c.closeClients()

	if sbCounter.closes != 1 {
		t.Errorf("sbClient.Close() called %d times, want 1", sbCounter.closes)
	}
	if nbCounter.closes != 1 {
		t.Errorf("nbClient.Close() called %d times, want 1", nbCounter.closes)
	}
	if c.sbClient != nil {
		t.Error("sbClient should be nil after closeClients()")
	}
	if c.nbClient != nil {
		t.Error("nbClient should be nil after closeClients()")
	}
}

func TestCloseClients_OnlySBSet(t *testing.T) {
	// Simulates the partial-failure case where SB was created but NB
	// creation failed before assignment. closeClients() must not panic.
	c := NewOVNClient(Config{}, nil)
	sb := &closeCountingClient{}
	c.sbClient = sb

	c.closeClients()

	if sb.closes != 1 {
		t.Errorf("sbClient.Close() called %d times, want 1", sb.closes)
	}
	if c.sbClient != nil {
		t.Error("sbClient should be nil after closeClients()")
	}
}

// TestCloseWithoutLoopDone verifies that Close() does not block waiting for
// a refresh loop that was never started. closeClients() is still invoked so
// the OVSDB clients are released.
func TestCloseWithoutLoopDone(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	sbCounter := &closeCountingClient{fakeOVSDBClient: sb}
	nbCounter := &closeCountingClient{fakeOVSDBClient: nb}
	c.sbClient = sbCounter
	c.nbClient = nbCounter
	c.ready.Store(true)

	done := make(chan struct{})
	go func() {
		c.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked even though loopDone is nil")
	}
	if c.ready.Load() {
		t.Error("Close should mark ready=false")
	}
	if sbCounter.closes != 1 || nbCounter.closes != 1 {
		t.Errorf("Close should call Close() on each client once, got sb=%d nb=%d",
			sbCounter.closes, nbCounter.closes)
	}
}

// TestCloseWaitsForLoopDone verifies that Close() blocks until the refresh
// loop has signalled completion by closing loopDone, then tears down clients.
func TestCloseWaitsForLoopDone(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	sbCounter := &closeCountingClient{fakeOVSDBClient: sb}
	nbCounter := &closeCountingClient{fakeOVSDBClient: nb}
	c.sbClient = sbCounter
	c.nbClient = nbCounter
	c.loopDone = make(chan struct{})

	closeDone := make(chan struct{})
	go func() {
		c.Close()
		close(closeDone)
	}()

	// Close must still be blocked because loopDone is open.
	select {
	case <-closeDone:
		t.Fatal("Close returned before loopDone was closed")
	case <-time.After(100 * time.Millisecond):
	}

	close(c.loopDone)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after loopDone closed")
	}
	if sbCounter.closes != 1 || nbCounter.closes != 1 {
		t.Errorf("expected one Close() per client, got sb=%d nb=%d",
			sbCounter.closes, nbCounter.closes)
	}
}

func TestCloseClients_Idempotent(t *testing.T) {
	// Both Connect()'s defer and a later explicit Close() can run on the
	// same OVNClient. Calling closeClients() twice must be safe.
	c, nb, sb := newOVNClientWithFakes(t, "host-a")
	sbCounter := &closeCountingClient{fakeOVSDBClient: sb}
	nbCounter := &closeCountingClient{fakeOVSDBClient: nb}
	c.sbClient = sbCounter
	c.nbClient = nbCounter

	c.closeClients()
	c.closeClients()

	if sbCounter.closes != 1 {
		t.Errorf("sbClient.Close() called %d times, want 1", sbCounter.closes)
	}
	if nbCounter.closes != 1 {
		t.Errorf("nbClient.Close() called %d times, want 1", nbCounter.closes)
	}
}

func TestSBDatabaseModel(t *testing.T) {
	m, err := sbDatabaseModel()
	if err != nil {
		t.Fatalf("sbDatabaseModel() error: %v", err)
	}
	if m.Name() != "OVN_Southbound" {
		t.Errorf("model name = %q, want %q", m.Name(), "OVN_Southbound")
	}
}

func TestNBDatabaseModel(t *testing.T) {
	m, err := nbDatabaseModel()
	if err != nil {
		t.Fatalf("nbDatabaseModel() error: %v", err)
	}
	if m.Name() != "OVN_Northbound" {
		t.Errorf("model name = %q, want %q", m.Name(), "OVN_Northbound")
	}
}

// startRefreshLoop spawns the OVNClient refresh loop for tests. The returned
// done channel is closed when the loop exits, so callers can cancel ctx and
// wait for a clean shutdown.
func startRefreshLoop(c *OVNClient, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.refreshLoop(ctx)
	}()
	return done
}

// waitRefreshSettled polls until refreshCount has been stable for two
// consecutive samples and the immediate signal channel is empty, indicating
// no further refresh is queued or in flight.
func waitRefreshSettled(t *testing.T, c *OVNClient, refreshCount *atomic.Int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var prev int64 = -1
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		cur := refreshCount.Load()
		if cur == prev && len(c.immediateCh) == 0 {
			return
		}
		prev = cur
	}
	t.Fatalf("refresh did not settle in time")
}

// TestImmediateStateRefreshCoalesces verifies that a storm of concurrent
// chassisredirect events does not spawn one goroutine per event. Instead,
// at most one refresh runs at a time and at most one follow-up is queued,
// so a 50-event burst produces 1 or 2 refreshState passes total.
func TestImmediateStateRefreshCoalesces(t *testing.T) {
	c, _, sb := newOVNClientWithFakes(t, "node1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ready.Store(true)

	var refreshCount atomic.Int64
	c.onChange = func() { refreshCount.Add(1) }

	// Block the very first List call so the in-flight refresh is held open
	// while the rest of the storm fires. Subsequent List calls run normally.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sb.onList = func() {
		once.Do(func() {
			close(started)
			<-release
		})
	}

	loopDone := startRefreshLoop(c, ctx)

	const events = 50
	var wg sync.WaitGroup
	wg.Add(events)
	for i := 0; i < events; i++ {
		go func() {
			defer wg.Done()
			c.immediateStateRefresh()
		}()
	}

	// Wait for the first refresh to enter refreshState, then give the
	// remaining 49 callers time to enqueue (they should all collapse into
	// the single pending slot).
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		wg.Wait()
		cancel()
		<-loopDone
		t.Fatal("first refresh never started")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	waitRefreshSettled(t, c, &refreshCount)
	cancel()
	<-loopDone

	n := refreshCount.Load()
	if n < 1 || n > 2 {
		t.Errorf("expected 1 or 2 refresh passes, got %d", n)
	}
}

// TestIsChassisRedirect covers the SB event-handler's port-binding filter.
// It returns true only for SB Port_Binding rows whose Type == "chassisredirect".
func TestIsChassisRedirect(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	h := &sbEventHandler{ovn: c}

	tests := []struct {
		name  string
		table string
		model model.Model
		want  bool
	}{
		{
			"port binding with chassisredirect type",
			"Port_Binding",
			&SBPortBinding{Type: "chassisredirect", LogicalPort: "cr-lrp-abc"},
			true,
		},
		{
			"port binding with patch type",
			"Port_Binding",
			&SBPortBinding{Type: "patch"},
			false,
		},
		{
			"chassis table is never a chassisredirect",
			"Chassis",
			&SBChassis{Name: "ch-a"},
			false,
		},
		{
			"wrong model type for Port_Binding",
			"Port_Binding",
			&SBChassis{Name: "ch-a"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.isChassisRedirect(tt.table, tt.model); got != tt.want {
				t.Errorf("isChassisRedirect(%q, %T) = %v, want %v",
					tt.table, tt.model, got, tt.want)
			}
		})
	}
}

// TestSBEventHandlerChassisRedirectSignalsDrainWatch verifies that the SB
// event handler wakes a drain wait on chassisredirect Port_Binding changes:
// OnAdd/OnUpdate/OnDelete each enqueue a coalesced drainWatchCh signal when
// the client is ready, a non-chassisredirect model does not, and nothing is
// enqueued before the client is ready.
func TestSBEventHandlerChassisRedirectSignalsDrainWatch(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	h := &sbEventHandler{ovn: c}
	cr := &SBPortBinding{Type: "chassisredirect", LogicalPort: "cr-lrp-abc"}

	// Not ready: no signal is queued even for a chassisredirect change.
	c.ready.Store(false)
	h.OnUpdate("Port_Binding", cr, cr)
	if got := len(c.drainWatchCh); got != 0 {
		t.Fatalf("drainWatchCh len before ready = %d, want 0", got)
	}

	// Ready: each event handler enqueues at most one coalesced signal.
	c.ready.Store(true)
	h.OnAdd("Port_Binding", cr)
	h.OnUpdate("Port_Binding", cr, cr)
	h.OnDelete("Port_Binding", cr)
	if got := len(c.drainWatchCh); got != 1 {
		t.Fatalf("drainWatchCh len after chassisredirect events = %d, want 1 (coalesced)", got)
	}

	// Consume the signal; a non-chassisredirect change must not re-arm it.
	<-c.drainWatchCh
	h.OnUpdate("Port_Binding", &SBPortBinding{Type: "patch"}, &SBPortBinding{Type: "patch"})
	if got := len(c.drainWatchCh); got != 0 {
		t.Fatalf("drainWatchCh len after non-chassisredirect change = %d, want 0", got)
	}
}

// TestDrainSignalsEmptiesBothChannels verifies that drainSignals consumes a
// pending signal on each channel without blocking when they are empty.
func TestDrainSignalsEmptiesBothChannels(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")

	// Pre-fill both signal channels (cap = 1).
	c.debounceCh <- struct{}{}
	c.immediateCh <- struct{}{}
	if len(c.debounceCh) != 1 || len(c.immediateCh) != 1 {
		t.Fatalf("setup: expected both channels to hold one signal, got %d/%d",
			len(c.debounceCh), len(c.immediateCh))
	}

	c.drainSignals()

	if got := len(c.debounceCh); got != 0 {
		t.Errorf("debounceCh len after drain = %d, want 0", got)
	}
	if got := len(c.immediateCh); got != 0 {
		t.Errorf("immediateCh len after drain = %d, want 0", got)
	}

	// Calling drainSignals on empty channels must not block.
	c.drainSignals()
}

// TestDebounceStateRefreshIgnoresNotReady verifies that signals are dropped
// before Connect() has marked the client ready. This is the production
// contract that prevents event handlers from triggering work on a
// half-initialised client.
func TestDebounceStateRefreshIgnoresNotReady(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	// ready defaults to false; explicit for clarity.
	c.ready.Store(false)

	c.debounceStateRefresh()
	if got := len(c.debounceCh); got != 0 {
		t.Errorf("debounceCh should remain empty when not ready, got len = %d", got)
	}
}

// TestDebounceStateRefreshQueuesAndCoalesces verifies the buffered-send
// semantics: the first call queues, subsequent calls coalesce until the
// channel is drained.
func TestDebounceStateRefreshQueuesAndCoalesces(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.ready.Store(true)

	c.debounceStateRefresh()
	if got := len(c.debounceCh); got != 1 {
		t.Fatalf("expected debounceCh len = 1 after first signal, got %d", got)
	}

	// A burst of additional calls must not block: the buffered slot is full
	// and the function takes the default branch.
	for i := 0; i < 20; i++ {
		c.debounceStateRefresh()
	}
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("debounceCh should coalesce, got len = %d, want 1", got)
	}

	// Drain and verify a fresh signal can be queued again.
	<-c.debounceCh
	c.debounceStateRefresh()
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("debounceCh should accept a new signal after drain, got len = %d", got)
	}
}

// TestSBEventHandlerOnAddImmediateForChassisRedirect verifies that a
// chassisredirect Port_Binding add triggers the immediate refresh path,
// bypassing debouncing.
func TestSBEventHandlerOnAddImmediateForChassisRedirect(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.ready.Store(true)
	h := &sbEventHandler{ovn: c}

	h.OnAdd("Port_Binding", &SBPortBinding{Type: "chassisredirect"})
	if got := len(c.immediateCh); got != 1 {
		t.Errorf("immediateCh len = %d, want 1", got)
	}
	if got := len(c.debounceCh); got != 0 {
		t.Errorf("debounceCh should be untouched, got len = %d", got)
	}
}

// TestSBEventHandlerOnAddDebouncesForOtherTables verifies that ordinary
// table changes (Chassis, non-chassisredirect Port_Binding) take the
// debounced refresh path.
func TestSBEventHandlerOnAddDebouncesForOtherTables(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.ready.Store(true)
	h := &sbEventHandler{ovn: c}

	h.OnAdd("Port_Binding", &SBPortBinding{Type: "patch"})
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("debounceCh len after patch port add = %d, want 1", got)
	}
	if got := len(c.immediateCh); got != 0 {
		t.Errorf("immediateCh should be untouched, got len = %d", got)
	}

	// Drain and try a chassis change.
	<-c.debounceCh
	h.OnAdd("Chassis", &SBChassis{Name: "ch-a"})
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("debounceCh len after chassis add = %d, want 1", got)
	}
}

func TestSBEventHandlerOnUpdateAndOnDelete(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.ready.Store(true)
	h := &sbEventHandler{ovn: c}

	// chassisredirect update → immediate.
	h.OnUpdate("Port_Binding",
		&SBPortBinding{Type: "chassisredirect"},
		&SBPortBinding{Type: "chassisredirect"})
	if got := len(c.immediateCh); got != 1 {
		t.Errorf("OnUpdate(chassisredirect) immediateCh len = %d, want 1", got)
	}
	<-c.immediateCh

	// chassisredirect delete → immediate.
	h.OnDelete("Port_Binding", &SBPortBinding{Type: "chassisredirect"})
	if got := len(c.immediateCh); got != 1 {
		t.Errorf("OnDelete(chassisredirect) immediateCh len = %d, want 1", got)
	}
	<-c.immediateCh

	// Non-chassisredirect update → debounce.
	h.OnUpdate("Chassis", &SBChassis{Name: "ch-a"}, &SBChassis{Name: "ch-a"})
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("OnUpdate(Chassis) debounceCh len = %d, want 1", got)
	}
	<-c.debounceCh

	// Non-chassisredirect delete → debounce.
	h.OnDelete("Chassis", &SBChassis{Name: "ch-a"})
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("OnDelete(Chassis) debounceCh len = %d, want 1", got)
	}
}

func TestSBEventHandlerHandleChangeFiltersUnrelatedTables(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.ready.Store(true)
	h := &sbEventHandler{ovn: c}

	// An unrelated SB table should not trigger any signal.
	h.handleChange("Datapath_Binding")
	if got := len(c.debounceCh) + len(c.immediateCh); got != 0 {
		t.Errorf("unrelated table signalled refresh: deb=%d imm=%d",
			len(c.debounceCh), len(c.immediateCh))
	}
}

func TestNBEventHandlerOnlyRelevantTablesTriggerDebounce(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.ready.Store(true)
	h := &nbEventHandler{ovn: c}

	// Gateway_Chassis is included: a peer's drain/restore/boost is a
	// Gateway_Chassis write, and the agent must wake so the following
	// reconcile runs EnsureActivePriorityLead and resolves any priority tie.
	tables := []string{"NAT", "Logical_Router", "Logical_Router_Port", "Gateway_Chassis"}
	for _, table := range tables {
		// Drain first so each iteration starts empty.
		select {
		case <-c.debounceCh:
		default:
		}
		h.OnAdd(table, nil)
		if got := len(c.debounceCh); got != 1 {
			t.Errorf("OnAdd(%q) debounceCh len = %d, want 1", table, got)
		}
	}

	// Drain.
	select {
	case <-c.debounceCh:
	default:
	}

	// Unrelated NB table — must not signal.
	h.OnAdd("Static_MAC_Binding", nil)
	if got := len(c.debounceCh); got != 0 {
		t.Errorf("OnAdd(Static_MAC_Binding) should not debounce, got len = %d", got)
	}

	// OnUpdate and OnDelete should also fan into handleChange.
	h.OnUpdate("NAT", nil, nil)
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("OnUpdate(NAT) debounceCh len = %d, want 1", got)
	}
	<-c.debounceCh
	h.OnDelete("Logical_Router_Port", nil)
	if got := len(c.debounceCh); got != 1 {
		t.Errorf("OnDelete(Logical_Router_Port) debounceCh len = %d, want 1", got)
	}
}

// TestRefreshStatePopulatesLocalRoutersAndNATs exercises the bulk of
// refreshState end-to-end with fake clients. It verifies that the mapping
// chain (SB chassisredirect → NB LRP → NB router → NB NAT) produces the
// expected derived state for a chassis with one locally-active router.
func TestRefreshStatePopulatesLocalRoutersAndNATs(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setRows("Chassis",
		&SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"},
		&SBChassis{UUID: "ch-b", Name: "ch-b", Hostname: "host-b"},
	)

	chA := "ch-a"
	chB := "ch-b"
	sb.setRows("Port_Binding",
		// Local chassisredirect — counts.
		&SBPortBinding{
			UUID:        "pb-1",
			LogicalPort: "cr-lrp-local",
			Type:        "chassisredirect",
			Chassis:     &chA,
		},
		// Remote chassisredirect — must be ignored.
		&SBPortBinding{
			UUID:        "pb-2",
			LogicalPort: "cr-lrp-remote",
			Type:        "chassisredirect",
			Chassis:     &chB,
		},
		// Patch port for the gateway with NatAddresses (SB SNAT discovery path).
		// The NAT IP is within the discovered network filter so it survives
		// the effectiveFilters check in refreshState.
		&SBPortBinding{
			UUID:        "pb-3",
			LogicalPort: "external-port",
			Type:        "patch",
			Options:     map[string]string{"peer": "lrp-local"},
			ExternalIDs: map[string]string{"neutron:device_owner": "network:router_gateway"},
			NatAddresses: []string{
				"fa:16:3e:11:22:33 198.51.100.60 is_chassis_resident(\"cr-lrp-local\")",
			},
		},
	)

	nb.setRows("Logical_Router_Port",
		&NBLogicalRouterPort{
			UUID:     "lrp-uuid-local",
			Name:     "lrp-local",
			MAC:      "fa:16:3e:aa:aa:aa",
			Networks: []string{"198.51.100.1/24"},
		},
		&NBLogicalRouterPort{
			UUID:     "lrp-uuid-remote",
			Name:     "lrp-remote",
			MAC:      "fa:16:3e:bb:bb:bb",
			Networks: []string{"203.0.113.1/24"},
		},
	)
	nb.setRows("Logical_Router",
		&NBLogicalRouter{
			UUID:  "lr-local",
			Name:  "router-local",
			Ports: []string{"lrp-uuid-local"},
			Nat:   []string{"nat-fip", "nat-snat"},
		},
		&NBLogicalRouter{
			UUID:  "lr-remote",
			Name:  "router-remote",
			Ports: []string{"lrp-uuid-remote"},
			Nat:   []string{"nat-remote"},
		},
	)
	nb.setRows("NAT",
		&NBNAT{UUID: "nat-fip", Type: "dnat_and_snat", ExternalIP: "198.51.100.50"},
		&NBNAT{UUID: "nat-snat", Type: "snat", ExternalIP: "198.51.100.51"},
		&NBNAT{UUID: "nat-remote", Type: "snat", ExternalIP: "203.0.113.50"},
	)

	c.state.SetLocalChassisName("host-a")
	c.refreshState(context.Background())

	snap := c.GetState()

	if !snap.HasLocalRouters {
		t.Fatal("HasLocalRouters should be true")
	}
	if len(snap.LocalRouters) != 1 {
		t.Fatalf("LocalRouters length = %d, want 1", len(snap.LocalRouters))
	}
	if snap.LocalRouters[0].RouterName != "router-local" {
		t.Errorf("LocalRouters[0].RouterName = %q, want %q",
			snap.LocalRouters[0].RouterName, "router-local")
	}

	// The FIP (dnat_and_snat external IP) must be tracked in the
	// IP→router-MAC map, which carries every NAT external IP.
	if _, ok := snap.NATIPToRouterMAC["198.51.100.50"]; !ok {
		t.Errorf("NATIPToRouterMAC = %v, missing FIP 198.51.100.50", snap.NATIPToRouterMAC)
	}
	// SB-derived SNAT (198.51.100.60) and NB-derived SNAT (198.51.100.51).
	wantSNATs := map[string]bool{"198.51.100.51": true, "198.51.100.60": true}
	gotSNATs := map[string]bool{}
	for _, ip := range snap.SNATIPs {
		gotSNATs[ip] = true
	}
	if !reflect.DeepEqual(gotSNATs, wantSNATs) {
		t.Errorf("SNATIPs = %v, want %v", gotSNATs, wantSNATs)
	}

	// Discovered network from the LRP.
	if got := snap.DiscoveredNetworks; len(got) != 1 || got[0].String() != "198.51.100.0/24" {
		var s []string
		for _, n := range got {
			s = append(s, n.String())
		}
		t.Errorf("DiscoveredNetworks = %v, want [198.51.100.0/24]", s)
	}

	// AllChassisNames should reflect both chassis from the SB Chassis table.
	if !snap.AllChassisNames["host-a"] || !snap.AllChassisNames["host-b"] {
		t.Errorf("AllChassisNames = %v, missing host-a or host-b", snap.AllChassisNames)
	}

	// NATIPToRouterMAC should map both local NAT IPs to the local LRP MAC.
	if snap.NATIPToRouterMAC["198.51.100.50"] != "fa:16:3e:aa:aa:aa" {
		t.Errorf("NATIPToRouterMAC[FIP] = %q, want %q",
			snap.NATIPToRouterMAC["198.51.100.50"], "fa:16:3e:aa:aa:aa")
	}
}

// emptyFilterLocalRouterFakes wires an OVNClient whose sole locally-active
// router has an LRP with no parseable networks, so refreshState discovers no
// network filters and computes an empty effectiveFilters set. This is the
// standby/edge condition weakness #3 of issue #158 describes, where NAT
// external IPs previously reached the command payloads unvalidated.
func emptyFilterLocalRouterFakes(t *testing.T, nats ...*NBNAT) *OVNClient {
	t.Helper()
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})
	chA := "ch-a"
	sb.setRows("Port_Binding",
		&SBPortBinding{UUID: "pb-1", LogicalPort: "cr-lrp-local", Type: "chassisredirect", Chassis: &chA},
	)
	nb.setRows("Logical_Router_Port",
		// Empty Networks → no discovered CIDRs → empty effectiveFilters.
		&NBLogicalRouterPort{UUID: "lrp-uuid-local", Name: "lrp-local", MAC: "fa:16:3e:aa:aa:aa"},
	)
	natUUIDs := make([]string, 0, len(nats))
	rows := make([]any, 0, len(nats))
	for _, n := range nats {
		natUUIDs = append(natUUIDs, n.UUID)
		rows = append(rows, n)
	}
	nb.setRows("NAT", rows...)
	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-local", Name: "router-local", Ports: []string{"lrp-uuid-local"}, Nat: natUUIDs},
	)

	c.state.SetLocalChassisName("host-a")
	c.refreshState(context.Background())
	return c
}

// TestRefreshStateDropsMalformedNATExternalIP proves a NAT row with an
// unparseable external_ip is dropped at ingest even when no filter is in
// effect: the raw string must not survive into SNATIPs, FIPs, or
// NATIPToRouterMAC (weakness #3, AC "nft/vtysh payloads only ever contain
// values that passed net.ParseIP").
func TestRefreshStateDropsMalformedNATExternalIP(t *testing.T) {
	c := emptyFilterLocalRouterFakes(t,
		&NBNAT{UUID: "nat-fip", Type: "dnat_and_snat", ExternalIP: "198.51.100.50"},
		&NBNAT{UUID: "nat-snat", Type: "snat", ExternalIP: "198.51.100.51"},
		&NBNAT{UUID: "nat-bad", Type: "snat", ExternalIP: "not-an-ip"},
	)
	snap := c.GetState()

	// Precondition for the weakness: no filters were derived.
	if len(snap.DiscoveredNetworks) != 0 {
		t.Fatalf("expected empty DiscoveredNetworks, got %v", snap.DiscoveredNetworks)
	}
	if _, ok := snap.NATIPToRouterMAC["198.51.100.50"]; !ok {
		t.Errorf("NATIPToRouterMAC = %v, missing FIP 198.51.100.50", snap.NATIPToRouterMAC)
	}
	if got := snap.SNATIPs; !reflect.DeepEqual(got, []string{"198.51.100.51"}) {
		t.Errorf("SNATIPs = %v, want [198.51.100.51] (malformed row must be dropped)", got)
	}
	if _, ok := snap.NATIPToRouterMAC["not-an-ip"]; ok {
		t.Errorf("NATIPToRouterMAC must not contain the malformed external IP: %v", snap.NATIPToRouterMAC)
	}
}

// TestRefreshStateKeepsV6ForHairpinButNotSNATIPs proves the family gate on the
// nft-bound SNATIPs list is independent of filter presence: with empty filters,
// a v6 SNAT and a v6 FIP are still tracked in NATIPToRouterMAC (the dual-stack
// OVS hairpin plane, #54) but never land in SNATIPs (the IPv4-only nft
// masquerade set). Covers steps 5 and 5b's family gate.
func TestRefreshStateKeepsV6ForHairpinButNotSNATIPs(t *testing.T) {
	c := emptyFilterLocalRouterFakes(t,
		&NBNAT{UUID: "nat-v4snat", Type: "snat", ExternalIP: "198.51.100.51"},
		&NBNAT{UUID: "nat-v6snat", Type: "snat", ExternalIP: "2001:db8::51"},
		&NBNAT{UUID: "nat-v6fip", Type: "dnat_and_snat", ExternalIP: "2001:db8::50"},
	)
	snap := c.GetState()

	if got := snap.SNATIPs; !reflect.DeepEqual(got, []string{"198.51.100.51"}) {
		t.Errorf("SNATIPs = %v, want [198.51.100.51] (v6 must be excluded)", got)
	}
	// The v6 addresses remain available for the family-aware OVS hairpin
	// plane — including the v6 FIP (dnat_and_snat), which the dual-stack
	// NATIPToRouterMAC map tracks even though it never enters the IPv4-only
	// SNATIPs set.
	for _, ip := range []string{"2001:db8::51", "2001:db8::50"} {
		if snap.NATIPToRouterMAC[ip] != "fa:16:3e:aa:aa:aa" {
			t.Errorf("NATIPToRouterMAC[%s] = %q, want the LRP MAC (v6 kept for hairpin)",
				ip, snap.NATIPToRouterMAC[ip])
		}
	}
}

// TestRefreshStateKeepsV6SBNatAddressForHairpinButNotSNATIPs drives the same
// family gate through the SB gateway NatAddresses path (step 5b): a v6 SNAT
// address discovered from a Port_Binding's NatAddresses stays in
// NATIPToRouterMAC for the dual-stack OVS hairpin plane (#54) but never lands
// in the IPv4-only nft masquerade set (SNATIPs, #85/#70). The LRP carries no
// networks, so effectiveFilters is empty and the To4() gate is the only thing
// separating the two addresses.
func TestRefreshStateKeepsV6SBNatAddressForHairpinButNotSNATIPs(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})
	chA := "ch-a"
	sb.setRows("Port_Binding",
		&SBPortBinding{UUID: "pb-cr", LogicalPort: "cr-lrp-local", Type: "chassisredirect", Chassis: &chA},
		&SBPortBinding{
			UUID: "pb-patch", LogicalPort: "ext", Type: "patch",
			Options:     map[string]string{"peer": "lrp-local"},
			ExternalIDs: map[string]string{"neutron:device_owner": "network:router_gateway"},
			NatAddresses: []string{
				"fa:16:3e:11:22:33 198.51.100.60 is_chassis_resident(\"cr-lrp-local\")",
				"fa:16:3e:11:22:33 2001:db8::60 is_chassis_resident(\"cr-lrp-local\")",
			},
		},
	)
	nb.setRows("Logical_Router_Port",
		// Empty Networks → no discovered CIDRs → empty effectiveFilters.
		&NBLogicalRouterPort{UUID: "lrp-uuid-local", Name: "lrp-local", MAC: "fa:16:3e:aa:aa:aa"},
	)
	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-local", Name: "router-local", Ports: []string{"lrp-uuid-local"}},
	)

	c.state.SetLocalChassisName("host-a")
	c.refreshState(context.Background())
	snap := c.GetState()

	if got := snap.SNATIPs; !reflect.DeepEqual(got, []string{"198.51.100.60"}) {
		t.Errorf("SNATIPs = %v, want [198.51.100.60] (v6 SB SNAT must be excluded)", got)
	}
	// The v6 address stays available for the family-aware OVS hairpin plane.
	if snap.NATIPToRouterMAC["2001:db8::60"] != "fa:16:3e:aa:aa:aa" {
		t.Errorf("NATIPToRouterMAC[2001:db8::60] = %q, want the LRP MAC (v6 kept for hairpin)",
			snap.NATIPToRouterMAC["2001:db8::60"])
	}
}

// TestRefreshStateResolvesLocalnetSegments verifies the SB-driven segment
// resolution: each locally-active router is associated with the localnet
// segment (localnet port + physnet + optional VLAN tag) of its external
// network, and NAT IPs inherit the owning router's segment. A router whose
// external switch has no localnet row keeps a nil segment — the trigger for
// the legacy single-patch-port fallback.
func TestRefreshStateResolvesLocalnetSegments(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setRows("Chassis", &SBChassis{UUID: "ch-a", Name: "ch-a", Hostname: "host-a"})

	chA := "ch-a"
	tag101 := 101
	sb.setRows("Port_Binding",
		// Three locally-active routers.
		&SBPortBinding{UUID: "pb-cr-vlan", LogicalPort: "cr-lrp-vlan", Type: "chassisredirect", Chassis: &chA},
		&SBPortBinding{UUID: "pb-cr-flat", LogicalPort: "cr-lrp-flat", Type: "chassisredirect", Chassis: &chA},
		&SBPortBinding{UUID: "pb-cr-none", LogicalPort: "cr-lrp-none", Type: "chassisredirect", Chassis: &chA},
		// VLAN segment: switch-side patch row + localnet row share dp-vlan.
		// The patch row also carries a NatAddresses SNAT IP (step 5b) that
		// must inherit the segment.
		&SBPortBinding{
			UUID: "pb-patch-vlan", LogicalPort: "ext-vlan", Type: "patch",
			Datapath:    "dp-vlan",
			Options:     map[string]string{"peer": "lrp-vlan"},
			ExternalIDs: map[string]string{"neutron:device_owner": "network:router_gateway"},
			NatAddresses: []string{
				"fa:16:3e:11:22:33 198.51.100.60 is_chassis_resident(\"cr-lrp-vlan\")",
			},
		},
		&SBPortBinding{
			UUID: "pb-ln-vlan", LogicalPort: "seg-vlan", Type: "localnet",
			Datapath: "dp-vlan",
			Tag:      &tag101,
			Options:  map[string]string{"network_name": "physnet1"},
		},
		// Flat segment: localnet row without a tag.
		&SBPortBinding{
			UUID: "pb-patch-flat", LogicalPort: "ext-flat", Type: "patch",
			Datapath: "dp-flat",
			Options:  map[string]string{"peer": "lrp-flat"},
		},
		&SBPortBinding{
			UUID: "pb-ln-flat", LogicalPort: "seg-flat", Type: "localnet",
			Datapath: "dp-flat",
			Options:  map[string]string{"network_name": "physnet1"},
		},
		// No localnet row at all for lrp-none's external switch.
		&SBPortBinding{
			UUID: "pb-patch-none", LogicalPort: "ext-none", Type: "patch",
			Datapath: "dp-none",
			Options:  map[string]string{"peer": "lrp-none"},
		},
	)

	nb.setRows("Logical_Router_Port",
		&NBLogicalRouterPort{UUID: "lrp-uuid-vlan", Name: "lrp-vlan", MAC: "fa:16:3e:aa:aa:01", Networks: []string{"198.51.100.1/24"}},
		&NBLogicalRouterPort{UUID: "lrp-uuid-flat", Name: "lrp-flat", MAC: "fa:16:3e:aa:aa:02", Networks: []string{"192.0.2.1/24"}},
		&NBLogicalRouterPort{UUID: "lrp-uuid-none", Name: "lrp-none", MAC: "fa:16:3e:aa:aa:03", Networks: []string{"203.0.113.1/24"}},
	)
	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-vlan", Name: "router-vlan", Ports: []string{"lrp-uuid-vlan"}, Nat: []string{"nat-vlan"}},
		&NBLogicalRouter{UUID: "lr-flat", Name: "router-flat", Ports: []string{"lrp-uuid-flat"}, Nat: []string{"nat-flat"}},
		&NBLogicalRouter{UUID: "lr-none", Name: "router-none", Ports: []string{"lrp-uuid-none"}, Nat: []string{"nat-none"}},
	)
	nb.setRows("NAT",
		&NBNAT{UUID: "nat-vlan", Type: "dnat_and_snat", ExternalIP: "198.51.100.50"},
		&NBNAT{UUID: "nat-flat", Type: "dnat_and_snat", ExternalIP: "192.0.2.50"},
		&NBNAT{UUID: "nat-none", Type: "dnat_and_snat", ExternalIP: "203.0.113.50"},
	)

	c.state.SetLocalChassisName("host-a")
	c.refreshState(context.Background())
	snap := c.GetState()

	if len(snap.LocalRouters) != 3 {
		t.Fatalf("LocalRouters length = %d, want 3", len(snap.LocalRouters))
	}
	segByRouter := map[string]*LocalnetSegment{}
	for i := range snap.LocalRouters {
		segByRouter[snap.LocalRouters[i].RouterName] = snap.LocalRouters[i].Segment
	}

	vlanSeg := segByRouter["router-vlan"]
	if vlanSeg == nil {
		t.Fatal("router-vlan has no segment")
	}
	if vlanSeg.LocalnetPort != "seg-vlan" || vlanSeg.NetworkName != "physnet1" {
		t.Errorf("router-vlan segment = %+v, want seg-vlan/physnet1", vlanSeg)
	}
	if vlanSeg.VLANTag == nil || *vlanSeg.VLANTag != 101 {
		t.Errorf("router-vlan VLANTag = %v, want 101", vlanSeg.VLANTag)
	}

	flatSeg := segByRouter["router-flat"]
	if flatSeg == nil {
		t.Fatal("router-flat has no segment")
	}
	if flatSeg.LocalnetPort != "seg-flat" || flatSeg.VLANTag != nil {
		t.Errorf("router-flat segment = %+v, want seg-flat with nil tag", flatSeg)
	}

	if segByRouter["router-none"] != nil {
		t.Errorf("router-none segment = %+v, want nil (no localnet row)", segByRouter["router-none"])
	}

	wantSegments := map[string]string{
		"198.51.100.50": "seg-vlan",
		"192.0.2.50":    "seg-flat",
		"198.51.100.60": "seg-vlan", // SB NatAddresses SNAT IP (step 5b)
	}
	for ip, want := range wantSegments {
		if got := snap.NATIPToSegment[ip]; got != want {
			t.Errorf("NATIPToSegment[%s] = %q, want %q", ip, got, want)
		}
	}
	if got, ok := snap.NATIPToSegment["203.0.113.50"]; ok {
		t.Errorf("NATIPToSegment[203.0.113.50] = %q, want absent (unresolved segment)", got)
	}

	// The snapshot must be a copy, not a reference.
	snap.NATIPToSegment["198.51.100.50"] = "modified"
	if c.GetState().NATIPToSegment["198.51.100.50"] == "modified" {
		t.Error("GetState should return a copy of NATIPToSegment, not a reference")
	}
}

func TestRefreshStateNoLocalRouters(t *testing.T) {
	c, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setRows("Chassis", &SBChassis{UUID: "ch-b", Name: "ch-b", Hostname: "host-b"})
	chB := "ch-b"
	sb.setRows("Port_Binding", &SBPortBinding{
		UUID:        "pb-remote",
		LogicalPort: "cr-lrp-remote",
		Type:        "chassisredirect",
		Chassis:     &chB,
	})
	nb.setRows("Logical_Router_Port",
		&NBLogicalRouterPort{UUID: "lrp-r", Name: "lrp-remote", Networks: []string{"203.0.113.1/24"}})
	nb.setRows("Logical_Router",
		&NBLogicalRouter{UUID: "lr-r", Name: "router-r", Ports: []string{"lrp-r"}})

	c.state.SetLocalChassisName("host-a")
	c.refreshState(context.Background())
	snap := c.GetState()

	if snap.HasLocalRouters {
		t.Errorf("HasLocalRouters should be false when no chassisredirect ports are local, got true")
	}
	if len(snap.LocalRouters) != 0 {
		t.Errorf("LocalRouters length = %d, want 0", len(snap.LocalRouters))
	}
}

// TestImmediateStateRefreshIgnoresNotReady mirrors the debounce test for the
// immediate channel: signals received before Connect() marks ready=true must
// be dropped silently to avoid spurious refreshes on a half-initialised
// client.
func TestImmediateStateRefreshIgnoresNotReady(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "host-a")
	c.ready.Store(false)
	c.immediateStateRefresh()
	if got := len(c.immediateCh); got != 0 {
		t.Errorf("immediateCh should remain empty when not ready, got len = %d", got)
	}
}

// TestRefreshLoopDebouncePath drives the loop through the full debounce →
// refresh → onChange sequence, which is the production code path for
// non-chassisredirect events. Existing tests cover only the immediate
// channel; this test exercises the debounce + reconcile timers.
func TestRefreshLoopDebouncePath(t *testing.T) {
	c, _, _ := newOVNClientWithFakes(t, "node1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ready.Store(true)

	var refreshCount atomic.Int64
	c.onChange = func() { refreshCount.Add(1) }

	loopDone := startRefreshLoop(c, ctx)

	c.debounceStateRefresh()

	// Wait for debounce (500ms) + reconcile (100ms) timers to fire.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if refreshCount.Load() >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-loopDone

	if got := refreshCount.Load(); got < 1 {
		t.Errorf("expected at least one onChange after debounce, got %d", got)
	}
}

func TestRefreshStateSBListErrorAborts(t *testing.T) {
	c, _, sb := newOVNClientWithFakes(t, "host-a")
	sb.listErr = errors.New("connection refused")

	// Should return without panicking and without populating state.
	c.refreshState(context.Background())
	snap := c.GetState()
	if snap.HasLocalRouters {
		t.Error("HasLocalRouters should remain false on SB list error")
	}
}

// TestImmediateStateRefreshFollowUpRuns verifies that a request arriving
// while a refresh is in-flight triggers exactly one follow-up pass after
// the in-flight one completes — so the latest event is never lost.
func TestImmediateStateRefreshFollowUpRuns(t *testing.T) {
	c, _, sb := newOVNClientWithFakes(t, "node1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ready.Store(true)

	var refreshCount atomic.Int64
	c.onChange = func() { refreshCount.Add(1) }

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	sb.onList = func() {
		once.Do(func() {
			close(started)
			<-release
		})
	}

	loopDone := startRefreshLoop(c, ctx)

	c.immediateStateRefresh()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		close(release)
		cancel()
		<-loopDone
		t.Fatal("first refresh never started")
	}

	// Second request while first is in-flight: must be queued in immediateCh.
	c.immediateStateRefresh()
	if got := len(c.immediateCh); got != 1 {
		t.Errorf("second call should be queued, immediateCh len = %d, want 1", got)
	}

	close(release)

	waitRefreshSettled(t, c, &refreshCount)
	cancel()
	<-loopDone

	if got := refreshCount.Load(); got != 2 {
		t.Errorf("expected exactly 2 refresh passes (in-flight + follow-up), got %d", got)
	}
}

// TestWatchConnectionStateMirrorsConnected verifies watchConnectionState
// tracks each client's Connected() into the ovn_connection_state gauge in
// both directions — the drop-and-recover cycle a transient OVSDB outage
// produces. This is the unit-level analog of the reconnect integration
// scenario, which asserts the same 1->0->1 transition against a real server.
func TestWatchConnectionStateMirrorsConnected(t *testing.T) {
	m := withTestMetrics(t)
	_, nb, sb := newOVNClientWithFakes(t, "host-a")

	sb.setConnected(true)
	nb.setConnected(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchConnectionState(ctx, sb, nb, time.Millisecond)
	}()

	waitConnGauge(t, m, "sb", 1)
	waitConnGauge(t, m, "nb", 1)

	// A transient NB outage: the gauge must drop while SB stays up.
	nb.setConnected(false)
	waitConnGauge(t, m, "nb", 0)
	waitConnGauge(t, m, "sb", 1)

	// Recovery: the gauge must climb back to 1.
	nb.setConnected(true)
	waitConnGauge(t, m, "nb", 1)

	cancel()
	<-done
}

// waitConnGauge polls the ovn_connection_state{database=db} gauge in the test
// registry until it equals want or the timeout expires.
func waitConnGauge(t *testing.T, m *metricsRegistry, db string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last float64
	var found bool
	for time.Now().Before(deadline) {
		last, found = connGaugeValue(t, m, db)
		if found && last == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ovn_connection_state{database=%q} = %v (found=%v), want %v", db, last, found, want)
}

// connGaugeValue reads the ovn_connection_state gauge for the given database
// label from the test registry.
func connGaugeValue(t *testing.T, m *metricsRegistry, db string) (float64, bool) {
	t.Helper()
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather(): %v", err)
	}
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_ovn_connection_state" {
			continue
		}
		for _, item := range mf.GetMetric() {
			for _, l := range item.GetLabel() {
				if l.GetName() == "database" && l.GetValue() == db {
					return item.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}
