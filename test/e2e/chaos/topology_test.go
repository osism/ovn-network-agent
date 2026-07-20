package main

import (
	"net/netip"
	"os"
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
