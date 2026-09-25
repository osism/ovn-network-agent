// Command ovn-network-agent is an event-driven agent that keeps host
// routing, FRR, and nftables state in sync with OVN on gateway nodes of
// OVN-based OpenStack environments.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

var version = "dev"

func main() {
	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		if errors.Is(err, errVersionRequested) {
			fmt.Println("ovn-network-agent", version)
			os.Exit(0)
		}
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	// --check-config: the configuration parsed and validated above; report
	// success and exit without running the agent. Invalid configs already
	// exited 1 via the error path above.
	if cfg.CheckConfig {
		fmt.Println("configuration OK")
		return
	}

	setupLogging(cfg.LogLevel)

	// The OVN-vs-port-forward-only decision is made in config validation
	// (validateMode); main only reports the resulting mode.
	mode := "full"
	if cfg.PortForwardOnly {
		mode = "port-forward-only"
	}

	networkMode := "auto-discover from OVN"
	switch {
	case len(cfg.NetworkCIDRs) > 0:
		networkMode = "manual"
	case cfg.PortForwardOnly:
		networkMode = "none (port-forward-only)"
	}

	slog.Info("ovn-network-agent starting",
		"version", version,
		"mode", mode,
		"dry_run", cfg.DryRun,
		"cleanup_on_shutdown", cfg.CleanupOnShutdown,
		"drain_on_shutdown", cfg.DrainOnShutdown,
		"drain_timeout", cfg.DrainTimeout,
		"ovn_sb_remote", cfg.OVNSBRemote,
		"ovn_nb_remote", cfg.OVNNBRemote,
		"ovn_ssl_ca", cfg.OVNSSLCA,
		"ovn_ssl_cert", cfg.OVNSSLCert,
		"ovn_ssl_key", cfg.OVNSSLKey,
		"bridge_dev", cfg.BridgeDev,
		"vrf_name", cfg.VRFName,
		"veth_nexthop", cfg.VethNexthop,
		"network_cidrs", cfg.NetworkCIDRs,
		"network_mode", networkMode,
		"gateway_port", cfg.GatewayPort,
		"route_table_id", cfg.RouteTableID,
		"ovs_wrapper", cfg.OVSWrapper,
		"reconcile_interval", cfg.ReconcileInterval,
		"veth_leak_enabled", cfg.VethLeakEnabled,
		"frr_prefix_list", cfg.FRRPrefixList,
		"stale_chassis_grace_period", cfg.StaleChassisGracePeriod,
		"port_forwards", len(cfg.PortForwards),
		"metrics_listen", cfg.MetricsListen,
	)

	if cfg.DryRun {
		slog.Warn("running in dry-run mode, no routes will be added or removed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// SIGHUP gets its own channel: a reload request queued in sigCh's
	// one-slot buffer would make signal.Notify drop a following SIGTERM.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)

	if cfg.MetricsListen != "" {
		m := initMetricsForConfig(cfg)
		if err := startMetricsServer(ctx, cfg.MetricsListen, m); err != nil {
			slog.Error("failed to start metrics endpoint", "addr", cfg.MetricsListen, "error", err)
			os.Exit(1)
		}
	}

	// A reload re-runs the full loader with the original args, so the
	// file < env < flag precedence holds on reload exactly as at startup.
	agent, err := NewAgent(cfg, func() (Config, error) { return loadConfig(os.Args[1:]) })
	if err != nil {
		slog.Error("failed to create agent", "error", err)
		os.Exit(1)
	}

	go func() {
		for range hupCh {
			slog.Info("received SIGHUP, reloading configuration")
			agent.RequestReload()
		}
	}()

	go func() {
		sig := <-sigCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("agent exited with error", "error", err)
		os.Exit(1)
	}

	slog.Info("ovn-network-agent stopped")
}

// initMetricsForConfig builds the process-wide metrics registry for cfg. In
// port-forward-only mode there is no OVN client, so OVN connection state must
// not gate /readyz: ovnRequired is the negation of PortForwardOnly. Keeping the
// derivation here (rather than inline in main) makes it testable.
func initMetricsForConfig(cfg Config) *metricsRegistry {
	return initMetrics(!cfg.PortForwardOnly)
}

// logLevel is the level of the process-wide handler setupLogging installs. It
// is a LevelVar so a SIGHUP reload can change log_level on the running agent.
var logLevel slog.LevelVar

// parseLogLevel maps a log_level value to its slog level. Unknown values fall
// back to info, as they always have: log_level is not validated.
func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func setupLogging(level string) {
	logLevel.Set(parseLogLevel(level))

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: &logLevel,
	})
	slog.SetDefault(slog.New(handler))
}
