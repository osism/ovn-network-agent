package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const metricsNamespace = "ovn_network_agent"

// readinessState feeds the /readyz endpoint. ovnRequired is written once in
// initMetrics before the HTTP server starts and read-only afterwards; the
// remaining fields are updated concurrently by the reconcile loop and the
// OVN connection callbacks, hence atomic.
type readinessState struct {
	ovnRequired     bool
	nbConnected     atomic.Bool
	sbConnected     atomic.Bool
	reconcileRan    atomic.Bool
	lastReconcileOK atomic.Bool
}

// metricsRegistry holds the Prometheus collectors used by the agent. Tests
// can construct a fresh registry; production uses a process-wide singleton
// initialised by initMetrics(). All collectors are no-ops when metrics is
// nil, so instrumentation calls are safe before initialisation.
type metricsRegistry struct {
	registry *prometheus.Registry

	// Build metadata
	buildInfo *prometheus.GaugeVec

	// Reconcile metrics
	reconcileTotal      *prometheus.CounterVec
	reconcileDuration   prometheus.Histogram
	reconcileInProgress prometheus.Gauge

	// Desired-state metrics
	desiredIPs        prometheus.Gauge
	localRouters      prometheus.Gauge
	effectiveNetworks prometheus.Gauge
	localnetSegments  prometheus.Gauge
	announcedVIPs     prometheus.Gauge

	// Provider-VRF prerequisites
	vrfDefaultRoutePresent prometheus.Gauge

	// Route stability metrics
	routeReAddsTotal    *prometheus.CounterVec
	consecutiveReAdds   prometheus.Gauge
	inactiveRoutes      prometheus.Gauge
	nexthopRepairsTotal prometheus.Counter

	// Failover metrics
	failoverAnnounceDuration prometheus.Histogram

	// OVN connection state
	ovnConnectionState *prometheus.GaugeVec

	// Drain metrics
	drainDuration prometheus.Histogram
	drainTotal    *prometheus.CounterVec

	// Stale chassis cleanup
	staleChassisCleanupTotal *prometheus.CounterVec
	missingChassis           prometheus.Gauge

	// OVS flow planes
	hairpinFlowsDesired   prometheus.Gauge
	hairpinFlowsInstalled prometheus.Gauge
	ovsFlowApplyErrors    *prometheus.CounterVec

	// Readiness signals backing the /readyz endpoint
	readiness readinessState
}

// metrics is the process-wide registry. It is non-nil after initMetrics()
// returns successfully and remains nil otherwise; all helpers tolerate the
// nil case so callers do not need to guard every recording site.
var metrics *metricsRegistry

// initMetrics builds the process-wide metrics registry. Calling it twice
// would re-register collectors and panic, so callers must invoke it at most
// once. Returns the registry for the HTTP handler. ovnRequired gates the OVN
// connection checks in /readyz: it is false in port-forward-only mode, where
// there is no OVN client and OVN state must not gate readiness.
func initMetrics(ovnRequired bool) *metricsRegistry {
	m := newMetricsRegistry()
	m.readiness.ovnRequired = ovnRequired
	metrics = m
	return m
}

// newMetricsRegistry constructs a self-contained registry. Used by initMetrics
// for the process-wide instance and by tests for isolation.
func newMetricsRegistry() *metricsRegistry {
	reg := prometheus.NewRegistry()
	m := &metricsRegistry{
		registry: reg,

		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "build_info",
			Help:      "Always 1, labelled with the running agent version (from -ldflags \"-X main.version=…\").",
		}, []string{"version"}),

		reconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "reconcile_total",
			Help:      "Total reconcile cycles, labelled by trigger source.",
		}, []string{"trigger"}),

		reconcileDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "reconcile_duration_seconds",
			Help:      "Duration of a single reconcile cycle in seconds.",
			Buckets:   []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}),

		reconcileInProgress: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "reconcile_in_progress",
			Help:      "1 while a reconcile cycle is running, 0 otherwise.",
		}),

		desiredIPs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "desired_ips",
			Help:      "Number of unique IPs the agent currently wants routes for (FIPs, SNATs, port-forward VIPs).",
		}),

		localRouters: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "local_routers",
			Help:      "Number of OVN logical routers whose chassisredirect port is currently active on this chassis.",
		}),

		effectiveNetworks: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "effective_networks",
			Help:      "Number of effective network filters (manual config or auto-discovered).",
		}),

		localnetSegments: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "localnet_segments",
			Help:      "Number of localnet segments (patch ports) currently bound for locally-active routers.",
		}),

		routeReAddsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "route_readds_total",
			Help:      "Total routes re-added by post-change verification, labelled by route plane.",
		}, []string{"plane"}),

		consecutiveReAdds: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "consecutive_readds",
			Help:      "Number of consecutive reconcile cycles that required route re-adds. Sustained non-zero indicates persistent route instability.",
		}),

		inactiveRoutes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "inactive_routes",
			Help:      "Number of desired FIP/VIP routes that exist as FRR static routes but are not selected/installed — i.e. not advertised via BGP. Non-zero means those FIPs are unreachable from outside.",
		}),

		announcedVIPs: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "announced_vips",
			Help:      "Number of configured port-forward VIPs this node announces this cycle. Zero while the VIPs are dormant because the node hosts no local routers.",
		}),

		vrfDefaultRoutePresent: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "vrf_default_route_present",
			Help:      "1 when the provider VRF's routing table holds a default route, 0 when it does not. A 0 on a node that announces VIPs or hosts routers means traffic to destinations the VRF does not host is dropped inside it.",
		}),

		nexthopRepairsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "nexthop_repairs_total",
			Help:      "Total times the agent re-notified the kernel about the veth-provider address because zebra was missing the connected route for the veth next-hop. Non-zero means every FIP route had failed to resolve and was not advertised via BGP.",
		}),

		failoverAnnounceDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "failover_announce_seconds",
			Help:      "Time from observing a chassisredirect change to completing the BGP announce of the takeover FIP routes, in seconds. Measured on the takeover reconcile.",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 1.5, 2, 3, 5, 10},
		}),

		ovnConnectionState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "ovn_connection_state",
			Help:      "1 when the named OVN database client is connected, 0 otherwise.",
		}, []string{"database"}),

		drainDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: metricsNamespace,
			Name:      "drain_duration_seconds",
			Help:      "Duration of a gateway drain operation in seconds.",
			Buckets:   []float64{0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		}),

		drainTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "drain_total",
			Help:      "Total drain operations, labelled by outcome (completed, timeout, error, noop).",
		}, []string{"outcome"}),

		staleChassisCleanupTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "stale_chassis_cleanup_total",
			Help:      "Total stale chassis cleanup events, labelled by outcome (success, error).",
		}, []string{"outcome"}),

		missingChassis: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "missing_chassis",
			Help:      "Number of chassis currently tracked as missing from the SB Chassis table.",
		}),

		hairpinFlowsDesired: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "hairpin_flows_desired",
			Help:      "Number of locally-managed IPs the last reconcile wanted a same-chassis hairpin flow for (FIPs, SNAT IPs, router gateway IPs).",
		}),

		hairpinFlowsInstalled: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "hairpin_flows_installed",
			Help:      "Number of hairpin flows found on the provider bridge at the start of the last reconcile, before the agent touched them. Below desired means same-chassis peers cannot reach those IPs.",
		}),

		ovsFlowApplyErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace,
			Name:      "ovs_flow_apply_errors_total",
			Help:      "Total failed OVS flow mutations, labelled by flow plane (hairpin, mactweak).",
		}, []string{"plane"}),
	}

	reg.MustRegister(
		m.buildInfo,
		m.reconcileTotal,
		m.reconcileDuration,
		m.reconcileInProgress,
		m.desiredIPs,
		m.localRouters,
		m.effectiveNetworks,
		m.localnetSegments,
		m.announcedVIPs,
		m.vrfDefaultRoutePresent,
		m.routeReAddsTotal,
		m.consecutiveReAdds,
		m.inactiveRoutes,
		m.nexthopRepairsTotal,
		m.failoverAnnounceDuration,
		m.ovnConnectionState,
		m.drainDuration,
		m.drainTotal,
		m.staleChassisCleanupTotal,
		m.missingChassis,
		m.hairpinFlowsDesired,
		m.hairpinFlowsInstalled,
		m.ovsFlowApplyErrors,
	)

	// Initialise label series so they appear in /metrics with a zero value
	// from the first scrape, instead of materialising only on the first
	// observation.
	m.buildInfo.WithLabelValues(version).Set(1)
	m.reconcileTotal.WithLabelValues("event").Add(0)
	m.reconcileTotal.WithLabelValues("periodic").Add(0)
	m.reconcileTotal.WithLabelValues("startup").Add(0)
	m.routeReAddsTotal.WithLabelValues("kernel").Add(0)
	m.routeReAddsTotal.WithLabelValues("frr").Add(0)
	m.drainTotal.WithLabelValues("completed").Add(0)
	m.drainTotal.WithLabelValues("timeout").Add(0)
	m.drainTotal.WithLabelValues("error").Add(0)
	m.drainTotal.WithLabelValues("noop").Add(0)
	m.staleChassisCleanupTotal.WithLabelValues("success").Add(0)
	m.staleChassisCleanupTotal.WithLabelValues("error").Add(0)
	m.ovnConnectionState.WithLabelValues("nb").Set(0)
	m.ovnConnectionState.WithLabelValues("sb").Set(0)
	m.ovsFlowApplyErrors.WithLabelValues("hairpin").Add(0)
	m.ovsFlowApplyErrors.WithLabelValues("mactweak").Add(0)

	return m
}

// startMetricsServer starts a /metrics HTTP server on listenAddr and shuts it
// down when ctx is cancelled. Returns an error if the listener cannot bind;
// runtime errors after startup are logged. Safe to call only once per process.
func startMetricsServer(ctx context.Context, listenAddr string, m *metricsRegistry) error {
	if listenAddr == "" {
		return nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		Registry: m.registry,
	}))
	// /healthz is liveness-only by design: it reports 200 whenever the
	// process is up. Use /readyz for readiness (OVN connected, reconcile
	// succeeded).
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", readyzHandler(m))

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	slog.Info("metrics endpoint listening", "addr", listener.Addr().String())

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics server exited with error", "error", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("metrics server shutdown error", "error", err)
		}
	}()

	return nil
}

// readyzHandler reports whether the agent is functional, as opposed to the
// liveness-only /healthz: OVN NB and SB connected (unless running
// port-forward-only), at least one reconcile completed, and the last
// reconcile's route sync succeeded. Returns 200 "ok" when ready, 503 with
// one plain-text line per failing check otherwise.
func readyzHandler(m *metricsRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var failures []string
		if m.readiness.ovnRequired {
			if !m.readiness.nbConnected.Load() {
				failures = append(failures, "unready: ovn nb disconnected")
			}
			if !m.readiness.sbConnected.Load() {
				failures = append(failures, "unready: ovn sb disconnected")
			}
		}
		if !m.readiness.reconcileRan.Load() {
			failures = append(failures, "unready: awaiting first reconcile")
		} else if !m.readiness.lastReconcileOK.Load() {
			failures = append(failures, "unready: last reconcile failed")
		}

		if len(failures) > 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			for _, f := range failures {
				_, _ = w.Write([]byte(f + "\n"))
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	}
}

// =============================================================================
// Recording helpers — all are nil-safe so call sites do not need guards.
// =============================================================================

func recordReconcile(trigger string, duration time.Duration) {
	if metrics == nil {
		return
	}
	metrics.reconcileTotal.WithLabelValues(trigger).Inc()
	metrics.reconcileDuration.Observe(duration.Seconds())
}

func setReconcileInProgress(inProgress bool) {
	if metrics == nil {
		return
	}
	if inProgress {
		metrics.reconcileInProgress.Set(1)
	} else {
		metrics.reconcileInProgress.Set(0)
	}
}

func setDesiredState(desiredIPs, localRouters, effectiveNetworks int) {
	if metrics == nil {
		return
	}
	metrics.desiredIPs.Set(float64(desiredIPs))
	metrics.localRouters.Set(float64(localRouters))
	metrics.effectiveNetworks.Set(float64(effectiveNetworks))
}

func setLocalnetSegments(n int) {
	if metrics == nil {
		return
	}
	metrics.localnetSegments.Set(float64(n))
}

func setAnnouncedVIPs(n int) {
	if metrics == nil {
		return
	}
	metrics.announcedVIPs.Set(float64(n))
}

// setVRFDefaultRoute records whether the provider VRF holds a default route.
// It is deliberately not called when the check itself failed: an unanswerable
// question must leave the last real answer standing rather than report the
// absence it could not establish.
func setVRFDefaultRoute(present bool) {
	if metrics == nil {
		return
	}
	v := 0.0
	if present {
		v = 1
	}
	metrics.vrfDefaultRoutePresent.Set(v)
}

func recordRouteReAdds(frr, kernel int) {
	if metrics == nil {
		return
	}
	if frr > 0 {
		metrics.routeReAddsTotal.WithLabelValues("frr").Add(float64(frr))
	}
	if kernel > 0 {
		metrics.routeReAddsTotal.WithLabelValues("kernel").Add(float64(kernel))
	}
}

func setConsecutiveReAdds(n int) {
	if metrics == nil {
		return
	}
	metrics.consecutiveReAdds.Set(float64(n))
}

func recordNexthopRepair() {
	if metrics == nil {
		return
	}
	metrics.nexthopRepairsTotal.Inc()
}

func setInactiveRoutes(n int) {
	if metrics == nil {
		return
	}
	metrics.inactiveRoutes.Set(float64(n))
}

// recordFailoverAnnounce records the time from observing a chassisredirect
// change to completing the BGP announce of the takeover FIP routes.
func recordFailoverAnnounce(d time.Duration) {
	if metrics == nil {
		return
	}
	metrics.failoverAnnounceDuration.Observe(d.Seconds())
}

func setOVNConnectionState(database string, connected bool) {
	if metrics == nil {
		return
	}
	v := 0.0
	if connected {
		v = 1
	}
	metrics.ovnConnectionState.WithLabelValues(database).Set(v)
	switch database {
	case "nb":
		metrics.readiness.nbConnected.Store(connected)
	case "sb":
		metrics.readiness.sbConnected.Store(connected)
	}
}

func recordDrain(outcome string, duration time.Duration) {
	if metrics == nil {
		return
	}
	metrics.drainTotal.WithLabelValues(outcome).Inc()
	metrics.drainDuration.Observe(duration.Seconds())
}

func recordStaleChassisCleanup(outcome string, count int) {
	if metrics == nil {
		return
	}
	if count <= 0 {
		count = 1
	}
	metrics.staleChassisCleanupTotal.WithLabelValues(outcome).Add(float64(count))
}

func setMissingChassis(n int) {
	if metrics == nil {
		return
	}
	metrics.missingChassis.Set(float64(n))
}

// setHairpinFlowPlane records what one reconcile wanted of the hairpin flow
// plane and what it found there before touching it. Both gauges move together
// so a scrape can never read a fresh desired against a stale installed.
func setHairpinFlowPlane(desired, installed int) {
	if metrics == nil {
		return
	}
	metrics.hairpinFlowsDesired.Set(float64(desired))
	metrics.hairpinFlowsInstalled.Set(float64(installed))
}

// recordOVSFlowApplyError counts one failed flow mutation on the named plane
// (hairpin, mactweak).
func recordOVSFlowApplyError(plane string) {
	if metrics == nil {
		return
	}
	metrics.ovsFlowApplyErrors.WithLabelValues(plane).Inc()
}

// setLastReconcileStatus records the outcome of the most recent reconcile
// cycle for the /readyz endpoint.
func setLastReconcileStatus(ok bool) {
	if metrics == nil {
		return
	}
	metrics.readiness.reconcileRan.Store(true)
	metrics.readiness.lastReconcileOK.Store(ok)
}
