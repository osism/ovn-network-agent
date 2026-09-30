package main

import (
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTopologyPinsManagementAddresses guards the `mgmt-ipv4` pins in
// topology.clab.yml.
//
// A gateway's management address is its geneve encap IP (ENCAP_IP in
// gwnode-entrypoint.sh reads it off eth0), and SB's Encap table carries a
// unique index on (type, ip). Left to Docker's IPAM, an address is
// released when a container stops and handed back out in start order — so
// a fault that stops two gateways at once can restart them holding each
// other's address. Each ovn-controller then tries to claim the address
// the other chassis' stale Encap row still holds, both transactions abort
// on the index, and neither can go first. That deadlock is permanent: it
// is what left every probe red for the whole recovery budget in the
// 2026-07-18 nightly `double-failover` (issue #208).
//
// Pinning every node closes it, but only for as long as every node stays
// pinned — one unpinned gateway added later re-opens exactly the same
// hole, and the next occurrence would again look like an agent failover
// regression rather than a lab defect. Hence this test.
func TestTopologyPinsManagementAddresses(t *testing.T) {
	raw, err := os.ReadFile("../topology.clab.yml")
	if err != nil {
		t.Fatalf("read the lab topology: %v", err)
	}

	var doc struct {
		Mgmt struct {
			IPv4Subnet string `yaml:"ipv4-subnet"`
		} `yaml:"mgmt"`
		Topology struct {
			Nodes map[string]struct {
				MgmtIPv4 string `yaml:"mgmt-ipv4"`
			} `yaml:"nodes"`
		} `yaml:"topology"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the lab topology: %v", err)
	}

	subnet, err := netip.ParsePrefix(doc.Mgmt.IPv4Subnet)
	if err != nil {
		t.Fatalf("parse the management subnet %q: %v", doc.Mgmt.IPv4Subnet, err)
	}
	if len(doc.Topology.Nodes) == 0 {
		t.Fatal("the topology declares no nodes — the parse shape has drifted")
	}

	// The first address of the subnet is the Docker bridge's own gateway,
	// so a node claiming it would fail to deploy rather than deadlock.
	bridge := subnet.Masked().Addr().Next()

	owner := make(map[netip.Addr]string, len(doc.Topology.Nodes))
	for name, node := range doc.Topology.Nodes {
		if node.MgmtIPv4 == "" {
			t.Errorf("node %s does not pin mgmt-ipv4: Docker's IPAM may hand it a different address after a restart, "+
				"which for a gateway swaps its geneve encap IP and deadlocks SB's unique (type, ip) Encap index", name)
			continue
		}
		addr, err := netip.ParseAddr(node.MgmtIPv4)
		if err != nil {
			t.Errorf("node %s pins an unparseable mgmt-ipv4 %q: %v", name, node.MgmtIPv4, err)
			continue
		}
		if !subnet.Contains(addr) {
			t.Errorf("node %s pins mgmt-ipv4 %s, outside the declared subnet %s", name, addr, subnet)
			continue
		}
		if addr == bridge {
			t.Errorf("node %s pins mgmt-ipv4 %s, which is the management bridge's own address", name, addr)
			continue
		}
		if other, dup := owner[addr]; dup {
			t.Errorf("nodes %s and %s both pin mgmt-ipv4 %s — a duplicate encap IP is the very collision the pins exist to prevent",
				other, name, addr)
			continue
		}
		owner[addr] = name
	}
}

// TestTopologyComputeNodeIsAWorkloadOnlyChassis guards the chassis the chaos
// runner hosts its workloads on (issue #280).
//
// It has to be a real OVN chassis, so it runs the gateway image, and it has
// to run no agent, so it runs that image in the compute role. Above all it
// must never become a fault target: a lifecycle fault on the node that
// hosts the workloads darkens every probe for the whole hold and hides the
// failover the run measures. The engine draws its targets from
// gatewayNames(), which is derived from underlayLinks, so an underlay link
// on the node, or a fourth row in that table, would put the workloads back
// in the line of fire and change what every recorded seed replays.
func TestTopologyComputeNodeIsAWorkloadOnlyChassis(t *testing.T) {
	raw, err := os.ReadFile("../topology.clab.yml")
	if err != nil {
		t.Fatalf("read the lab topology: %v", err)
	}

	var doc struct {
		Topology struct {
			Nodes map[string]struct {
				Image string            `yaml:"image"`
				Env   map[string]string `yaml:"env"`
			} `yaml:"nodes"`
			Links []struct {
				Endpoints []string `yaml:"endpoints"`
			} `yaml:"links"`
		} `yaml:"topology"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the lab topology: %v", err)
	}
	gateway, ok := doc.Topology.Nodes["gateway-1"]
	if !ok || gateway.Image == "" || len(doc.Topology.Links) == 0 {
		t.Fatal("the topology declares no gateway-1 image or no links — the parse shape has drifted")
	}

	node, ok := doc.Topology.Nodes[workloadHost]
	if !ok {
		t.Fatalf("the topology declares no node %s to host the workloads", workloadHost)
	}
	if node.Image != gateway.Image {
		t.Errorf("%s runs image %q, want the gateways' %q: it has to be an OVN chassis like them",
			workloadHost, node.Image, gateway.Image)
	}
	if got := node.Env["GWNODE_ROLE"]; got != "compute" {
		t.Errorf("%s sets GWNODE_ROLE %q, want compute: any other role runs FRR and the agent", workloadHost, got)
	}
	if got := node.Env["CHASSIS_NAME"]; got != workloadHost {
		t.Errorf("%s registers as chassis %q, want %q", workloadHost, got, workloadHost)
	}
	for _, link := range doc.Topology.Links {
		for _, endpoint := range link.Endpoints {
			if strings.HasPrefix(endpoint, workloadHost+":") {
				t.Errorf("%s has a link endpoint %q: the workload host carries no underlay", workloadHost, endpoint)
			}
		}
	}

	want := []string{"gateway-1", "gateway-2", "gateway-3"}
	if got := gatewayNames(); !slices.Equal(got, want) {
		t.Fatalf("gatewayNames() = %v, want exactly %v: the engine draws its targets from it", got, want)
	}
	// An unset role is the gateway role; a gateway that set one would at
	// best repeat the default and at worst drop its agent.
	for _, gw := range want {
		if role, set := doc.Topology.Nodes[gw].Env["GWNODE_ROLE"]; set {
			t.Errorf("%s sets GWNODE_ROLE %q; the gateways rely on the default", gw, role)
		}
	}
}
