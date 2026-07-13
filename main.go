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

	if cfg.MetricsListen != "" {
		if err := startMetricsServer(ctx, cfg.MetricsListen, initMetrics()); err != nil {
			slog.Error("failed to start metrics endpoint", "addr", cfg.MetricsListen, "error", err)
			os.Exit(1)
		}
	}

	agent, err := NewAgent(cfg)
	if err != nil {
		slog.Error("failed to create agent", "error", err)
		os.Exit(1)
	}

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

func setupLogging(level string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
	})
	slog.SetDefault(slog.New(handler))
}
