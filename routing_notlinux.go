//go:build !linux

package main

import (
	"fmt"
	"log/slog"
	"net"
)

func (rm *RouteManager) CheckBridgeDevice() error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] skipping bridge device check", "dev", rm.cfg.BridgeDev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) EnsureBridgeIP(ip string) error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would add bridge IP", "ip", ip, "dev", rm.cfg.BridgeDev)
		return nil
	}
	return fmt.Errorf("bridge IP management is only supported on Linux")
}

func (rm *RouteManager) RemoveBridgeIP(ip string) error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would remove bridge IP", "ip", ip, "dev", rm.cfg.BridgeDev)
		return nil
	}
	return fmt.Errorf("bridge IP management is only supported on Linux")
}

func (rm *RouteManager) EnableProxyARP() error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would enable proxy ARP", "dev", rm.cfg.BridgeDev)
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
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would prune stale segment interfaces", "keep", len(keepTags))
		return nil
	}
	return fmt.Errorf("segment interface management is only supported on Linux")
}

func (rm *RouteManager) TeardownSegmentInterfaces() error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would remove agent-created segment interfaces")
		return nil
	}
	return fmt.Errorf("segment interface management is only supported on Linux")
}

func (rm *RouteManager) AddKernelRoute(ip, dev string) error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would add kernel route", "ip", ip, "dev", dev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) DelKernelRoute(ip, dev string) error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would remove kernel route", "ip", ip, "dev", dev)
		return nil
	}
	return fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) EnsureKernelRouteRule(ip string) error {
	// Non-Linux builds cannot own kernel routes, but unit tests use their
	// listKernelRoutes hook to model already-present routes. Treat policy-rule
	// repair as a no-op there so that model remains usable.
	return nil
}

func (rm *RouteManager) ListKernelRoutes() ([]kernelRouteEntry, error) {
	if rm.cfg.DryRun {
		return nil, nil
	}
	return nil, fmt.Errorf("kernel route management is only supported on Linux")
}

func (rm *RouteManager) CleanupRoutingTable() error {
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would flush routing table", "table", rm.cfg.RouteTableID)
		return nil
	}
	return fmt.Errorf("routing table management is only supported on Linux")
}

func (rm *RouteManager) ReconcileVethLeakNetworks(desired []*net.IPNet) error {
	if !rm.cfg.VethLeakEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would reconcile veth leak networks", "desired", len(desired))
		return nil
	}
	return fmt.Errorf("veth VRF leak is only supported on Linux")
}

func (rm *RouteManager) SetupVethLeak() error {
	if !rm.cfg.VethLeakEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would set up veth VRF leak")
		return nil
	}
	return fmt.Errorf("veth VRF leak is only supported on Linux")
}

func (rm *RouteManager) RefreshVethNexthop(networks []*net.IPNet) error {
	if !rm.cfg.VethLeakEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would refresh the veth-provider address", "networks", len(networks))
		return nil
	}
	return fmt.Errorf("veth VRF leak is only supported on Linux")
}

func (rm *RouteManager) TeardownVethLeak() error {
	if !rm.cfg.VethLeakEnabled {
		return nil
	}
	if rm.cfg.DryRun {
		slog.Info("[dry-run] would tear down veth VRF leak")
		return nil
	}
	return fmt.Errorf("veth VRF leak is only supported on Linux")
}

// VRFDefaultRoutePresent reports the dependency as satisfied rather than
// erroring, the only stub here that does. Its caller turns a false into an
// operator-facing ERROR about dropped traffic, and a non-Linux build has no VRF
// to route through in the first place — so reporting absence would be alarming
// about a data plane that does not exist, and reporting an error would put a
// permanent warning on every reconcile.
func (rm *RouteManager) VRFDefaultRoutePresent() (bool, error) {
	return true, nil
}
