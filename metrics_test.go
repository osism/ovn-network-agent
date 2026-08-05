package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// gaugeValue reads one unlabelled gauge back out of a registry. It fails the
// test when the metric is absent, so a renamed collector is a failure rather
// than a silent zero.
func gaugeValue(t *testing.T, m *metricsRegistry, name string) float64 {
	t.Helper()
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, mf := range got {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %s missing from the registry", name)
	return 0
}

// counterValue reads one label series of a counter vector back out of a
// registry, failing the test when the series is absent.
func counterValue(t *testing.T, m *metricsRegistry, name, label, value string) float64 {
	t.Helper()
	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}
	for _, mf := range got {
		if mf.GetName() != name {
			continue
		}
		for _, item := range mf.GetMetric() {
			for _, l := range item.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					return item.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("counter %s{%s=%q} missing from the registry", name, label, value)
	return 0
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
	setInactiveRoutes(0)
	recordFailoverAnnounce(750 * time.Millisecond)
	setOVNConnectionState("nb", true)
	recordDrain("completed", time.Second)
	recordStaleChassisCleanup("success", 2)
	setMissingChassis(1)
	setHairpinFlowPlane(3, 2)
	recordOVSFlowApplyError("hairpin")
	setLastReconcileStatus(true)
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
		"ovn_network_agent_build_info",
		"ovn_network_agent_reconcile_total",
		"ovn_network_agent_reconcile_duration_seconds",
		"ovn_network_agent_desired_ips",
		"ovn_network_agent_local_routers",
		"ovn_network_agent_localnet_segments",
		"ovn_network_agent_route_readds_total",
		"ovn_network_agent_consecutive_readds",
		"ovn_network_agent_inactive_routes",
		"ovn_network_agent_failover_announce_seconds",
		"ovn_network_agent_ovn_connection_state",
		"ovn_network_agent_drain_duration_seconds",
		"ovn_network_agent_drain_total",
		"ovn_network_agent_stale_chassis_cleanup_total",
		"ovn_network_agent_missing_chassis",
		"ovn_network_agent_hairpin_flows_desired",
		"ovn_network_agent_hairpin_flows_installed",
		"ovn_network_agent_ovs_flow_apply_errors_total",
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

func TestBuildInfoGaugeCarriesVersionLabel(t *testing.T) {
	m := newMetricsRegistry()

	got, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error: %v", err)
	}

	var found bool
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_build_info" {
			continue
		}
		found = true
		series := mf.GetMetric()
		if len(series) != 1 {
			t.Fatalf("build_info series count = %d, want 1", len(series))
		}
		item := series[0]
		var version string
		for _, l := range item.GetLabel() {
			if l.GetName() == "version" {
				version = l.GetValue()
			}
		}
		if version != "dev" {
			t.Errorf("build_info version label = %q, want %q", version, "dev")
		}
		if v := item.GetGauge().GetValue(); v != 1 {
			t.Errorf("build_info value = %v, want 1", v)
		}
	}
	if !found {
		t.Error("build_info gauge missing from registry")
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

	// /healthz is liveness-only and always 200 while the process is up.
	healthz, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = healthz.Body.Close() }()
	if healthz.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", healthz.StatusCode)
	}

	// /readyz on a fresh registry with no reconcile recorded is not ready.
	readyz, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer func() { _ = readyz.Body.Close() }()
	if readyz.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want 503", readyz.StatusCode)
	}
}

// callReadyz drives readyzHandler against the given registry and returns the
// response recorder so callers can assert on status and body.
func callReadyz(m *metricsRegistry) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	readyzHandler(m)(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rr
}

// TestReadyzNotReadyBeforeFirstReconcile proves a fresh agent that has not yet
// completed a reconcile reports 503, even with OVN checks disabled.
func TestReadyzNotReadyBeforeFirstReconcile(t *testing.T) {
	m := withTestMetrics(t) // ovnRequired stays false

	rr := callReadyz(m)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "awaiting first reconcile") {
		t.Errorf("body missing 'awaiting first reconcile'; got:\n%s", rr.Body.String())
	}
}

// TestReadyzReflectsReconcileOutcome proves readiness flips with the recorded
// reconcile outcome once at least one cycle has run.
func TestReadyzReflectsReconcileOutcome(t *testing.T) {
	m := withTestMetrics(t) // ovnRequired stays false

	setLastReconcileStatus(true)
	if rr := callReadyz(m); rr.Code != http.StatusOK || rr.Body.String() != "ok\n" {
		t.Errorf("after ok reconcile: status = %d body = %q, want 200 %q", rr.Code, rr.Body.String(), "ok\n")
	}

	setLastReconcileStatus(false)
	rr := callReadyz(m)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("after failed reconcile: status = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "last reconcile failed") {
		t.Errorf("body missing 'last reconcile failed'; got:\n%s", rr.Body.String())
	}
}

// TestReadyzRequiresOVNWhenConfigured proves that OVN connection state gates
// readiness only when ovnRequired is set.
func TestReadyzRequiresOVNWhenConfigured(t *testing.T) {
	t.Run("ovn required", func(t *testing.T) {
		m := withTestMetrics(t)
		m.readiness.ovnRequired = true
		setLastReconcileStatus(true)

		// nb/sb disconnected: both checks fail even though reconcile is OK.
		rr := callReadyz(m)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("disconnected: status = %d, want 503", rr.Code)
		}
		if body := rr.Body.String(); !strings.Contains(body, "nb disconnected") || !strings.Contains(body, "sb disconnected") {
			t.Errorf("body missing nb/sb disconnected lines; got:\n%s", body)
		}

		// Both connected: ready.
		setOVNConnectionState("nb", true)
		setOVNConnectionState("sb", true)
		if rr := callReadyz(m); rr.Code != http.StatusOK {
			t.Errorf("connected: status = %d, want 200", rr.Code)
		}

		// SB drops again: unready.
		setOVNConnectionState("sb", false)
		if rr := callReadyz(m); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("sb dropped: status = %d, want 503", rr.Code)
		}
	})

	t.Run("ovn not required ignores connection state", func(t *testing.T) {
		m := withTestMetrics(t) // ovnRequired stays false
		setLastReconcileStatus(true)
		setOVNConnectionState("nb", false)
		setOVNConnectionState("sb", false)

		if rr := callReadyz(m); rr.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 despite disconnected OVN", rr.Code)
		}
	})
}

// TestInitMetricsAssignsProcessRegistry verifies that initMetrics builds a
// fresh registry and assigns it to the process-wide `metrics` variable so
// recording helpers go through it.
func TestInitMetricsAssignsProcessRegistry(t *testing.T) {
	prev := metrics
	metrics = nil
	t.Cleanup(func() { metrics = prev })

	m := initMetrics(false)
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

// TestInitMetricsForConfigDerivesOVNRequired guards the config→ovnRequired
// mapping wired into main: flipping the negation would make /readyz gate on OVN
// connectivity in port-forward-only mode (perpetual 503) or ignore it in full
// mode (ready with OVN down). Both subtests go red if that derivation breaks.
func TestInitMetricsForConfigDerivesOVNRequired(t *testing.T) {
	prev := metrics
	t.Cleanup(func() { metrics = prev })

	t.Run("port-forward-only leaves OVN out of readiness", func(t *testing.T) {
		m := initMetricsForConfig(Config{PortForwardOnly: true})
		if m.readiness.ovnRequired {
			t.Error("ovnRequired = true in port-forward-only mode, want false")
		}
		// A successful reconcile is enough: /readyz is 200 with no OVN connection.
		setLastReconcileStatus(true)
		if rr := callReadyz(m); rr.Code != http.StatusOK {
			t.Errorf("/readyz = %d in port-forward-only mode, want 200", rr.Code)
		}
	})

	t.Run("full mode gates readiness on OVN", func(t *testing.T) {
		m := initMetricsForConfig(Config{PortForwardOnly: false})
		if !m.readiness.ovnRequired {
			t.Error("ovnRequired = false in full mode, want true")
		}
		// Reconcile OK but OVN never connected: /readyz stays 503.
		setLastReconcileStatus(true)
		if rr := callReadyz(m); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("/readyz = %d with OVN disconnected, want 503", rr.Code)
		}
	})
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

func TestRecordFailoverAnnounceObservesHistogram(t *testing.T) {
	m := withTestMetrics(t)
	recordFailoverAnnounce(900 * time.Millisecond)
	recordFailoverAnnounce(1500 * time.Millisecond)

	got, _ := m.registry.Gather()
	var found bool
	for _, mf := range got {
		if mf.GetName() != "ovn_network_agent_failover_announce_seconds" {
			continue
		}
		found = true
		h := mf.GetMetric()[0].GetHistogram()
		if c := h.GetSampleCount(); c != 2 {
			t.Errorf("failover_announce_seconds sample count = %d, want 2", c)
		}
		if sum := h.GetSampleSum(); sum < 2.39 || sum > 2.41 {
			t.Errorf("failover_announce_seconds sample sum = %v, want ~2.4", sum)
		}
	}
	if !found {
		t.Error("failover_announce_seconds histogram missing from registry")
	}
}

func TestRecordDrainCountsByOutcome(t *testing.T) {
	m := withTestMetrics(t)
	recordDrain("completed", 750*time.Millisecond)
	recordDrain("timeout", 5*time.Second)
	recordDrain("completed", 250*time.Millisecond)
	recordDrain("noop", 10*time.Millisecond)

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
		if seen["completed"] != 2 || seen["timeout"] != 1 || seen["noop"] != 1 {
			t.Errorf("drain_total = %v, want completed=2 timeout=1 noop=1", seen)
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

func TestSetHairpinFlowPlaneMovesBothGaugesTogether(t *testing.T) {
	m := withTestMetrics(t)
	setHairpinFlowPlane(4, 2)

	if v := gaugeValue(t, m, "ovn_network_agent_hairpin_flows_desired"); v != 4 {
		t.Errorf("hairpin_flows_desired = %v, want 4", v)
	}
	if v := gaugeValue(t, m, "ovn_network_agent_hairpin_flows_installed"); v != 2 {
		t.Errorf("hairpin_flows_installed = %v, want 2", v)
	}

	// The heal: the next cycle finds the plane whole again.
	setHairpinFlowPlane(4, 4)
	if v := gaugeValue(t, m, "ovn_network_agent_hairpin_flows_installed"); v != 4 {
		t.Errorf("hairpin_flows_installed = %v after the heal, want 4", v)
	}
}

// Both label series must exist at zero on a fresh registry, or the alert's
// `installed < desired` comparison would have nothing to evaluate until the
// first failure — and a scraper could not tell "no errors" from "no agent".
func TestOVSFlowApplyErrorSeriesStartAtZero(t *testing.T) {
	m := newMetricsRegistry()

	for _, plane := range []string{"hairpin", "mactweak"} {
		if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", plane); v != 0 {
			t.Errorf("ovs_flow_apply_errors_total{plane=%q} = %v on a fresh registry, want 0", plane, v)
		}
	}
}

func TestRecordOVSFlowApplyErrorCountsByPlane(t *testing.T) {
	m := withTestMetrics(t)
	recordOVSFlowApplyError("hairpin")
	recordOVSFlowApplyError("hairpin")
	recordOVSFlowApplyError("mactweak")

	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "hairpin"); v != 2 {
		t.Errorf("hairpin apply errors = %v, want 2", v)
	}
	if v := counterValue(t, m, "ovn_network_agent_ovs_flow_apply_errors_total", "plane", "mactweak"); v != 1 {
		t.Errorf("mactweak apply errors = %v, want 1", v)
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
