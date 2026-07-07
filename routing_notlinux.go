//go:build !linux

package main

import (
	"fmt"
	"log/slog"
	"net"
)

func (rm *RouteManager) CheckBridgeDevice() error {
	if rm.dryRun {
		slog.Info("[dry-run] skipping bridge device check", "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) EnsureBridgeIP(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would add bridge IP", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("bridge IP management is only supported on Linux")
}

func (rm *RouteManager) RemoveBridgeIP(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove bridge IP", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("bridge IP management is only supported on Linux")
}

func (rm *RouteManager) EnableProxyARP() error {
	if rm.dryRun {
		slog.Info("[dry-run] would enable proxy ARP", "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("proxy ARP is only supported on Linux")
}

func (rm *RouteManager) GetBridgeMAC() (net.HardwareAddr, error) {
	return nil, fmt.Errorf("bridge MAC lookup is only supported on Linux")
}

func (rm *RouteManager) EnsureSegmentInterface(tag int) (string, string, error) {
	return "", "", fmt.Errorf("segment interface management is only supported on Linux")
}

func (rm *RouteManager) PruneSegmentInterfaces(keepTags map[int]bool) error {
	if rm.dryRun {
		slog.Info("[dry-run] would prune stale segment interfaces", "keep", len(keepTags))
		return nil
	}
	return fmt.Errorf("segment interface management is only supported on Linux")
}

func (rm *RouteManager) TeardownSegmentInterfaces() error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove agent-created segment interfaces")
		return nil
	}
	return fmt.Errorf("segment interface management is only supported on Linux")
}

func (rm *RouteManager) AddKernelRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would add kernel route", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) DelKernelRoute(ip string) error {
	if rm.dryRun {
		slog.Info("[dry-run] would remove kernel route", "ip", ip, "dev", rm.bridgeDev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) ListKernelRoutes() ([]string, error) {
	if rm.dryRun {
		return nil, nil
	}
	return nil, fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) CleanupRoutingTable() error {
	if rm.dryRun {
		slog.Info("[dry-run] would flush routing table", "table", rm.routeTableID)
		return nil
	}
	return fmt.Errorf("routing table management is only supported on Linux")
}

func (rm *RouteManager) ReconcileVethLeakNetworks(desired []*net.IPNet) error {
	if !rm.vethLeakEnabled {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would reconcile veth leak networks", "desired", len(desired))
		return nil
	}
	return fmt.Errorf("veth VRF leak is only supported on Linux")
}

func (rm *RouteManager) SetupVethLeak() error {
	if !rm.vethLeakEnabled {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would set up veth VRF leak")
		return nil
	}
	return fmt.Errorf("veth VRF leak is only supported on Linux")
}

func (rm *RouteManager) TeardownVethLeak() error {
	if !rm.vethLeakEnabled {
		return nil
	}
	if rm.dryRun {
		slog.Info("[dry-run] would tear down veth VRF leak")
		return nil
	}
	return fmt.Errorf("veth VRF leak is only supported on Linux")
}
