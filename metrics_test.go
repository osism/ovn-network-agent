package main

import (
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// withTestMetrics swaps the process-wide metrics registry for a fresh one
// during the test and restores the previous value after the test ends.
// Returns the test registry so the caller can inspect it.
func withTestMetrics(t *testing.T) *metricsRegistry {
	t.Helper()
	prev := metrics
	m := newMetricsRegistry()
	metrics = m
	t.Cleanup(func() { metrics = prev })
	return m
}

func TestRecordingHelpersAreNilSafe(t *testing.T) {
	prev := metrics
	metrics = nil
	t.Cleanup(func() { metrics = prev })

	// None of these should panic when metrics is nil.
	recordReconcile("event", 100*time.Millisecond)
	setReconcileInProgress(true)
	setReconcileInProgress(false)
	setDesiredState(5, 2, 3)
	setLocalnetSegments(2)
	recordRouteReAdds(1, 2)
	setConsecutiveReAdds(4)
	setOVNConnectionState("nb", true)
	recordDrain("completed", time.Second)
	recordStaleChassisCleanup("success", 2)
	setMissingChassis(1)
	setBFDDetectSeconds("ovn", 3)
	recordBFDCheckError("ovn")
}

func TestNewMetricsRegistryRegistersAllCollectors(t *testing.T) {
	m := newMetricsRegistry()

	// Sanity: collectors are non-nil and the registry can produce a metric
	// family list.
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	wantMetrics := []string{
		"ovn_network_agent_reconcile_total",
		"ovn_network_agent_reconcile_duration_seconds",
		"ovn_network_agent_desired_ips",
		"ovn_network_agent_local_routers",
		"ovn_network_agent_localnet_segments",
		"ovn_network_agent_route_readds_total",
		"ovn_network_agent_consecutive_readds",
		"ovn_network_agent_inactive_routes",
		"ovn_network_agent_ovn_connection_state",
		"ovn_network_agent_drain_duration_seconds",
		"ovn_network_agent_drain_total",
		"ovn_network_agent_stale_chassis_cleanup_total",
		"ovn_network_agent_missing_chassis",
		"ovn_network_agent_bfd_detect_seconds",
		"ovn_network_agent_bfd_check_errors_total",
	}
	gotNames := make(map[string]bool, len(got))
	for _, mf := range got {
		gotNames[mf.GetName()] = true
	}
	for _, name := range wantMetrics {
		if !gotNames[name] {
			t.Errorf("metric %s missing from registry", name)
		}
	}
}

func TestRecordReconcileUpdatesCounterAndHistogram(t *testing.T) {
	m := withTestMetrics(t)

	recordReconcile("event", 250*time.Millisecond)
	recordReconcile("periodic", 50*time.Millisecond)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_reconcile_total" {
			continue
		}
		var event, periodic float64
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != "trigger" {
					continue
				}
				switch l.GetValue() {
				case "event":
					event = m.GetCounter().GetValue()
				case "periodic":
					periodic = m.GetCounter().GetValue()
				}
			}
		}
		if event != 1 || periodic != 1 {
			t.Errorf("reconcile_total counters: event=%v, periodic=%v, want 1/1", event, periodic)
		}
	}
}

func TestSetOVNConnectionStateTogglesGauge(t *testing.T) {
	m := withTestMetrics(t)

	setOVNConnectionState("sb", true)
	setOVNConnectionState("nb", false)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_ovn_connection_state" {
			continue
		}
		for _, item := range mf.GetMetric() {
			var db string
			for _, l := range item.GetLabel() {
				if l.GetName() == "database" {
					db = l.GetValue()
				}
			}
			val := item.GetGauge().GetValue()
			switch db {
			case "sb":
				if val != 1 {
					t.Errorf("sb state = %v, want 1", val)
				}
			case "nb":
				if val != 0 {
					t.Errorf("nb state = %v, want 0", val)
				}
			}
		}
	}
}

func TestStartMetricsServerServesMetricsEndpoint(t *testing.T) {
	m := withTestMetrics(t)

	// Bind to an OS-assigned port to avoid collisions in CI.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close() // immediately free; startMetricsServer rebinds

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := startMetricsServer(ctx, addr, m); err != nil {
		t.Fatalf("startMetricsServer: %v", err)
	}

	// Record a metric so the endpoint has something visible to scrape.
	recordReconcile("event", 100*time.Millisecond)

	// Poll briefly for the listener to be ready.
	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, err := http.Get("http://" + addr + "/metrics")
		if err == nil {
			resp = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("metrics endpoint never became reachable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "ovn_network_agent_reconcile_total") {
		t.Errorf("body missing reconcile_total; got:\n%s", body)
	}
}

// TestInitMetricsAssignsProcessRegistry verifies that initMetrics builds a
// fresh registry and assigns it to the process-wide `metrics` variable so
// recording helpers go through it.
func TestInitMetricsAssignsProcessRegistry(t *testing.T) {
	prev := metrics
	metrics = nil
	t.Cleanup(func() { metrics = prev })

	m := initMetrics()
	if m == nil {
		t.Fatal("initMetrics returned nil")
	}
	if metrics != m {
		t.Errorf("process-wide metrics not assigned: metrics = %p, want %p", metrics, m)
	}
	if m.registry == nil {
		t.Error("returned registry has no underlying prometheus.Registry")
	}
}

func TestSetReconcileInProgressTogglesGauge(t *testing.T) {
	m := withTestMetrics(t)

	setReconcileInProgress(true)
	setReconcileInProgress(false)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_reconcile_in_progress" {
			continue
		}
		if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 0 {
			t.Errorf("reconcile_in_progress = %v after setting to false, want 0", v)
		}
	}
}

func TestSetDesiredStateUpdatesGauges(t *testing.T) {
	m := withTestMetrics(t)
	setDesiredState(7, 3, 2)

	got, _ := m.registry.Gather()
	wantValues := map[string]float64{
		"ovn_network_agent_desired_ips":        7,
		"ovn_network_agent_local_routers":      3,
		"ovn_network_agent_effective_networks": 2,
	}
	for _, mf := range got {
		want, ok := wantValues[mf.GetName()]
		if !ok {
			continue
		}
		if v := mf.GetMetric()[0].GetGauge().GetValue(); v != want {
			t.Errorf("%s = %v, want %v", mf.GetName(), v, want)
		}
	}
}

// TestSetLocalnetSegmentsSetsGauge verifies the gauge tracks the current
// segment count, including the reset back to zero when the last local
// router (and with it every bound segment) disappears.
func TestSetLocalnetSegmentsSetsGauge(t *testing.T) {
	m := withTestMetrics(t)
	setLocalnetSegments(2)
	setLocalnetSegments(0)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_localnet_segments" {
			continue
		}
		if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 0 {
			t.Errorf("localnet_segments = %v after reset, want 0", v)
		}
	}
}

func TestRecordRouteReAddsAddsLabelledCounters(t *testing.T) {
	m := withTestMetrics(t)
	recordRouteReAdds(2, 5) // frr=2, kernel=5

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_route_readds_total" {
			continue
		}
		seen := map[string]float64{}
		for _, item := range mf.GetMetric() {
			for _, l := range item.GetLabel() {
				if l.GetName() == "plane" {
					seen[l.GetValue()] = item.GetCounter().GetValue()
				}
			}
		}
		if seen["frr"] != 2 || seen["kernel"] != 5 {
			t.Errorf("route_readds_total = %v, want frr=2 kernel=5", seen)
		}
	}
}

func TestRecordRouteReAddsSkipsZeroValues(t *testing.T) {
	m := withTestMetrics(t)
	// Counters are pre-initialised to 0 in newMetricsRegistry. Calling with
	// zero counts must not increment either label.
	recordRouteReAdds(0, 0)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_route_readds_total" {
			continue
		}
		for _, item := range mf.GetMetric() {
			if v := item.GetCounter().GetValue(); v != 0 {
				t.Errorf("counter incremented despite zero input: %v", v)
			}
		}
	}
}

func TestSetConsecutiveReAddsSetsGauge(t *testing.T) {
	m := withTestMetrics(t)
	setConsecutiveReAdds(4)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_consecutive_readds" {
			continue
		}
		if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 4 {
			t.Errorf("consecutive_readds = %v, want 4", v)
		}
	}
}

func TestSetInactiveRoutesSetsGauge(t *testing.T) {
	m := withTestMetrics(t)
	setInactiveRoutes(3)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_inactive_routes" {
			continue
		}
		if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 3 {
			t.Errorf("inactive_routes = %v, want 3", v)
		}
	}
}

func TestRecordDrainCountsByOutcome(t *testing.T) {
	m := withTestMetrics(t)
	recordDrain("completed", 750*time.Millisecond)
	recordDrain("timeout", 5*time.Second)
	recordDrain("completed", 250*time.Millisecond)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_drain_total" {
			continue
		}
		seen := map[string]float64{}
		for _, item := range mf.GetMetric() {
			for _, l := range item.GetLabel() {
				if l.GetName() == "outcome" {
					seen[l.GetValue()] = item.GetCounter().GetValue()
				}
			}
		}
		if seen["completed"] != 2 || seen["timeout"] != 1 {
			t.Errorf("drain_total = %v, want completed=2 timeout=1", seen)
		}
	}
}

func TestRecordStaleChassisCleanupDefaultsCountToOne(t *testing.T) {
	m := withTestMetrics(t)
	recordStaleChassisCleanup("success", 0) // 0 should become 1
	recordStaleChassisCleanup("error", 3)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_stale_chassis_cleanup_total" {
			continue
		}
		seen := map[string]float64{}
		for _, item := range mf.GetMetric() {
			for _, l := range item.GetLabel() {
				if l.GetName() == "outcome" {
					seen[l.GetValue()] = item.GetCounter().GetValue()
				}
			}
		}
		if seen["success"] != 1 || seen["error"] != 3 {
			t.Errorf("cleanup_total = %v, want success=1 error=3", seen)
		}
	}
}

func TestSetMissingChassisSetsGauge(t *testing.T) {
	m := withTestMetrics(t)
	setMissingChassis(2)

	got, _ := m.registry.Gather()
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_missing_chassis" {
			continue
		}
		if v := mf.GetMetric()[0].GetGauge().GetValue(); v != 2 {
			t.Errorf("missing_chassis = %v, want 2", v)
		}
	}
}

func TestStartMetricsServerNoopWhenAddrEmpty(t *testing.T) {
	if err := startMetricsServer(context.Background(), "", newMetricsRegistry()); err != nil {
		t.Fatalf("expected nil error for empty addr, got %v", err)
	}
}

func TestStartMetricsServerErrorsOnInvalidAddr(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := startMetricsServer(ctx, "127.0.0.1:not-a-port", newMetricsRegistry()); err == nil {
		t.Fatal("expected error for invalid addr, got nil")
	}
}

// bfdDetectValue returns the gauge value for one layer of
// ovn_network_agent_bfd_detect_seconds, and whether the series exists.
func bfdDetectValue(t *testing.T, m *metricsRegistry, layer string) (float64, bool) {
	t.Helper()
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_bfd_detect_seconds" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "layer" && lp.GetValue() == layer {
					return metric.GetGauge().GetValue(), true
				}
			}
		}
	}
	return 0, false
}

// bfdCheckErrorCount returns the counter value for one layer of
// ovn_network_agent_bfd_check_errors_total.
func bfdCheckErrorCount(t *testing.T, m *metricsRegistry, layer string) float64 {
	t.Helper()
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_bfd_check_errors_total" {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "layer" && lp.GetValue() == layer {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("layer %q missing from bfd_check_errors_total", layer)
	return 0
}

// A layer whose state cannot be read has an unknown detection time, not a
// zero-second one. Counting the read failures is what lets an operator tell the
// two apart, because NaN satisfies no comparison an alert can express.
func TestRecordBFDCheckErrorCountsPerLayer(t *testing.T) {
	m := withTestMetrics(t)

	for _, layer := range []string{"ovn", "frr"} {
		if v := bfdCheckErrorCount(t, m, layer); v != 0 {
			t.Errorf("layer %q = %v on a fresh registry, want 0", layer, v)
		}
	}

	recordBFDCheckError("ovn")
	recordBFDCheckError("ovn")

	if v := bfdCheckErrorCount(t, m, "ovn"); v != 2 {
		t.Errorf("layer ovn = %v, want 2", v)
	}
	if v := bfdCheckErrorCount(t, m, "frr"); v != 0 {
		t.Errorf("layer frr = %v after two ovn errors, want 0", v)
	}
}

func TestSetBFDDetectSeconds(t *testing.T) {
	m := withTestMetrics(t)

	// Both layers are bootstrapped as NaN so they appear on the first scrape
	// without claiming the instantaneous — i.e. perfect — detection time that a
	// zero would assert before anything has been measured.
	for _, layer := range []string{"ovn", "frr"} {
		v, ok := bfdDetectValue(t, m, layer)
		if !ok {
			t.Fatalf("layer %q missing from a fresh registry", layer)
		}
		if !math.IsNaN(v) {
			t.Errorf("layer %q = %v on a fresh registry, want NaN", layer, v)
		}
	}

	setBFDDetectSeconds("ovn", 3)
	setBFDDetectSeconds("frr", 0.45)

	if v, _ := bfdDetectValue(t, m, "ovn"); v != 3 {
		t.Errorf("layer ovn = %v, want 3", v)
	}
	if v, _ := bfdDetectValue(t, m, "frr"); v != 0.45 {
		t.Errorf("layer frr = %v, want 0.45", v)
	}

	// Setting one layer must not disturb the other.
	setBFDDetectSeconds("ovn", 0.45)
	if v, _ := bfdDetectValue(t, m, "frr"); v != 0.45 {
		t.Errorf("layer frr = %v after updating ovn, want 0.45", v)
	}
}
