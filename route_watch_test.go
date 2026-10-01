package main

import (
	"net"
	"testing"
)

const (
	testWatchFIP     = "192.0.2.10"
	testWatchVIP     = "192.0.2.20"
	testWatchNexthop = "169.254.0.1"
	testWatchLeakNet = "192.0.2.0/24"
)

// testRouteOwnership owns one FIP as a kernel route and as an FRR static, one
// port-forward VIP as a kernel route only, and one veth-leak network. The VIP
// keeps the two sets apart, as they are in production.
func testRouteOwnership(t *testing.T) routeOwnership {
	t.Helper()
	_, leak, err := net.ParseCIDR(testWatchLeakNet)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", testWatchLeakNet, err)
	}
	return newRouteOwnership([]string{testWatchFIP, testWatchVIP}, []string{testWatchFIP}, []*net.IPNet{leak})
}

func TestClassifyRouteEvent(t *testing.T) {
	const (
		fipDst = testWatchFIP + "/32"
		vipDst = testWatchVIP + "/32"
	)
	owned := testRouteOwnership(t)

	cases := []struct {
		name      string
		ev        routeEvent
		owned     routeOwnership
		wantKind  string
		wantDrift bool
	}{
		{
			name:      "case 1: deleted agent /32 in the kernel table",
			ev:        routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: fipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "case 2: deleted agent leak network in the VRF table",
			ev:        routeEvent{Deleted: true, VRFTable: true, AgentProto: true, Dst: testWatchLeakNet, Gw: testWatchNexthop},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "case 3: deleted FRR static in the VRF table",
			ev:        routeEvent{Deleted: true, VRFTable: true, Dst: fipDst, Gw: testWatchNexthop},
			owned:     owned,
			wantKind:  "frr",
			wantDrift: true,
		},
		{
			name:      "case 4: foreign route replaced the agent /32 in the kernel table",
			ev:        routeEvent{Replaced: true, KernelTable: true, Dst: fipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "case 5: foreign route replaced the FRR static with another next hop",
			ev:        routeEvent{Replaced: true, VRFTable: true, Dst: fipDst, Gw: "198.51.100.1"},
			owned:     owned,
			wantKind:  "frr",
			wantDrift: true,
		},
		{
			name:      "both tables: a delete of the agent /32 is the kernel case, not the FRR case",
			ev:        routeEvent{Deleted: true, KernelTable: true, VRFTable: true, AgentProto: true, Dst: fipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "both tables: a foreign replace is the kernel case, not the FRR case",
			ev:        routeEvent{Replaced: true, KernelTable: true, VRFTable: true, Dst: fipDst, Gw: "198.51.100.1"},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:      "kernel-only VIP: a delete of the agent /32 in the kernel table",
			ev:        routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: vipDst},
			owned:     owned,
			wantKind:  "kernel",
			wantDrift: true,
		},
		{
			name:  "kernel-only VIP: a delete of a non-agent /32 in the VRF table",
			ev:    routeEvent{Deleted: true, VRFTable: true, Dst: vipDst, Gw: testWatchNexthop},
			owned: owned,
		},
		{
			name:  "kernel-only VIP: a foreign replace in the VRF table with another next hop",
			ev:    routeEvent{Replaced: true, VRFTable: true, Dst: vipDst, Gw: "198.51.100.1"},
			owned: owned,
		},
		{
			name:  "delete of a /32 that is not owned",
			ev:    routeEvent{Deleted: true, KernelTable: true, AgentProto: true, Dst: "192.0.2.99/32"},
			owned: owned,
		},
		{
			name:  "delete in the kernel table without the agent protocol",
			ev:    routeEvent{Deleted: true, KernelTable: true, Dst: fipDst},
			owned: owned,
		},
		{
			name:  "delete of a leak network that is not owned",
			ev:    routeEvent{Deleted: true, VRFTable: true, AgentProto: true, Dst: "198.51.100.0/24"},
			owned: owned,
		},
		{
			name:  "delete of the leak network without the agent protocol",
			ev:    routeEvent{Deleted: true, VRFTable: true, Dst: testWatchLeakNet},
			owned: owned,
		},
		{
			name:  "new route without replace",
			ev:    routeEvent{KernelTable: true, Dst: fipDst},
			owned: owned,
		},
		{
			name:  "replace by the agent itself",
			ev:    routeEvent{Replaced: true, KernelTable: true, AgentProto: true, Dst: fipDst},
			owned: owned,
		},
		{
			name:  "replace in the VRF table that keeps the veth next hop",
			ev:    routeEvent{Replaced: true, VRFTable: true, Dst: fipDst, Gw: testWatchNexthop},
			owned: owned,
		},
		{
			name:  "replace in the VRF table without a gateway",
			ev:    routeEvent{Replaced: true, VRFTable: true, Dst: fipDst},
			owned: owned,
		},
		{
			name: "zero-value ownership",
			ev:   routeEvent{Deleted: true, KernelTable: true, VRFTable: true, AgentProto: true, Dst: fipDst},
		},
		{
			name:  "empty ownership",
			ev:    routeEvent{Deleted: true, KernelTable: true, VRFTable: true, AgentProto: true, Dst: fipDst},
			owned: newRouteOwnership(nil, nil, nil),
		},
		{
			name:  "empty ownership, foreign replace",
			ev:    routeEvent{Replaced: true, KernelTable: true, VRFTable: true, Dst: fipDst, Gw: "198.51.100.1"},
			owned: newRouteOwnership(nil, nil, nil),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, drift := classifyRouteEvent(tc.ev, tc.owned, testWatchNexthop)
			if kind != tc.wantKind || drift != tc.wantDrift {
				t.Errorf("classifyRouteEvent() = (%q, %v), want (%q, %v)", kind, drift, tc.wantKind, tc.wantDrift)
			}
		})
	}
}
