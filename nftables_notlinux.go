//go:build !linux

package main

import (
	"fmt"
	"log/slog"
	"net"
)

func (rm *RouteManager) SetupPortForward() error {
	if !rm.cfg.PortForwardEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would set up port forwarding")
		return nil
	}
	return fmt.Errorf("port forwarding is only supported on Linux")
}

func (rm *RouteManager) ReconcilePortForward(providerNetworks []*net.IPNet, snatIPs []string, announceVIPs bool) error {
	if !rm.cfg.PortForwardEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would reconcile port forwarding")
		return nil
	}
	return fmt.Errorf("port forwarding is only supported on Linux")
}

func (rm *RouteManager) TeardownPortForward() error {
	if !rm.cfg.PortForwardEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would tear down port forwarding")
		return nil
	}
	return fmt.Errorf("port forwarding is only supported on Linux")
}
