package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// A chaos run used to exercise exactly one agent configuration — the one
// the gwnode image bakes in. A profile (issue #177) makes the
// configuration an input instead: it names both the start topology the
// run layers onto the bootstrap baseline and the agent configuration each
// gateway runs, as an overlay on test/e2e/gwnode-config.yaml. Profile and
// seed together determine a run, so the profile is part of the
// reproducibility contract and is journaled alongside the seed.
//
// The set below is curated rather than combinatorial: six configurations
// that each change what the agent actually has to do — no VLAN provider
// networks, DNAT on an API VIP, no OVN connection at all, gateways
// running different configurations mid-rollout — instead of every product
// of the agent's behaviour-changing options.

const (
	// defaultProfileName keeps today's chaos run: every topology layer
	// on, every gateway on the baked lab config.
	defaultProfileName = "everything-on"

	// The API VIP is the agent's *own* DNAT path (port_forwards), as
	// opposed to the OVN Load_Balancer VIP the port-forward layer puts
	// up. It sits inside the always-present provider network because the
	// agent reconciles the FRR prefix-list to exactly the effective
	// networks — a VIP outside every covered prefix is never announced,
	// and would be dark for the whole run. Its backend is the gateway's
	// own management address, where startAPIBackend runs a responder.
	apiVIPAddr = "192.0.2.80"
	apiVIPPort = 8080
	apiVIPURL  = "http://192.0.2.80:8080/"

	// flipVIPAddr is the VIP the pf-rule-toggle flip adds and removes.
	// It is deliberately neither the API VIP nor probed: a rule that
	// exists on one gateway and not on another must never draw probe
	// traffic to a gateway that has no backend behind it.
	flipVIPAddr = "192.0.2.99"
	flipVIPPort = 8081

	// The hairpin VIP is the API VIP's counterpart for a client *inside*
	// OVN. It cannot share the API VIP's address: 192.0.2.80 sits in the
	// provider subnet, which is a connected route on lr0, so OVN would
	// deliver VIP traffic on the public logical switch and ARP for it
	// there — nothing answers, and the chassis kernel's DNAT never sees
	// the packet (the trap pf-hairpin.sh documents). Out of every subnet
	// the lab connects, it follows lr0's default route out through
	// cr-lr0-public onto br-ex instead, where the MAC-tweak flow hands it
	// to the kernel. 198.18.0.0/15 is the RFC 2544 benchmark range; all
	// three TEST-NET ranges are already lab networks.
	hairpinVIPAddr = "198.18.0.50"
	hairpinVIPPort = 8080
)

// hairpinVIPHostPort is the probeTCP target's address.
var hairpinVIPHostPort = fmt.Sprintf("%s:%d", hairpinVIPAddr, hairpinVIPPort)

// explicitCIDRs is the manual network filter the profiles and the
// cidr-toggle flip switch a gateway to. It covers every provider network
// the lab probes — the bootstrap public net plus the two VLAN nets — and
// both VIPs, so swapping OVN-discovered networks for a manual filter
// exercises the filter codepath without darkening a probe.
var explicitCIDRs = []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"}

// The probe targets the start-state layers put up. Which of them a run
// measures is the profile's call: a layer that is off has no responder
// behind its target, so probing it would be red for the whole run.
var (
	probeVM1     = probeTarget{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"}
	probeVM2     = probeTarget{name: "fip-vm2", kind: probePing, addr: "192.0.2.12"}
	probeVLAN101 = probeTarget{name: "fip-vlan101", kind: probePing, addr: "198.51.100.10"}
	probeVLAN102 = probeTarget{name: "fip-vlan102", kind: probePing, addr: "203.0.113.10"}
	probeLBVIP   = probeTarget{name: "pf-vip", kind: probeHTTP, addr: vipURL}
	probeAPIVIP  = probeTarget{name: "api-vip", kind: probeHTTP, addr: apiVIPURL}

	// The two same-node vantages, both from vm1's namespace on the
	// workload host. Every target above is measured from client-1, which
	// reaches a FIP over the physical network — the path that keeps
	// working when the hairpin plane is broken. These two ride the
	// cookie-0x998 reflect path on whichever chassis holds cr-lr0-public,
	// and go dark exactly when it has a hole.
	probeHairpinFIP = probeTarget{
		name: "hairpin-fip", kind: probePing, addr: "192.0.2.12",
		node: workloadHost, netns: "vm1",
	}
	probeHairpinVIP = probeTarget{
		name: "hairpin-vip", kind: probeTCP, addr: hairpinVIPHostPort,
		node: workloadHost, netns: "vm1",
	}

	// The cross-chassis vantage (#265): vm3 sits behind lr1, whose
	// chassisredirect port is pinned to gateway-2, and pings the FIP behind
	// lr0. The packet leaves OVN on gateway-2, crosses that kernel's veth
	// path into vrf-provider, rides BGP to whichever chassis holds
	// cr-lr0-public and enters that kernel on veth-default — the one
	// ingress no other probe exercises, and the one the veth-leak source
	// rule used to loop.
	probeCrossFIP = probeTarget{
		name: "cross-fip", kind: probePing, addr: "192.0.2.10",
		node: workloadHost, netns: "vm3",
	}
)

// defaultProbes is what a full-topology run measures: the four FIPs, the
// OVN Load_Balancer VIP, the same-node hairpin FIP, and the cross-chassis
// FIP.
//
// The baseline lab's second FIP, 192.0.2.11, is deliberately absent:
// bootstrap.sh seeds its NAT row but nothing answers behind
// 192.168.10.11, so probing it would be red for the whole run.
var defaultProbes = []probeTarget{
	probeVM1, probeVM2, probeVLAN101, probeVLAN102, probeLBVIP, probeHairpinFIP,
	probeCrossFIP,
}

// gwConfig is a profile's agent-configuration overlay for one gateway,
// layered key by key over the baked lab config. The zero value changes
// nothing — which is what keeps a fresh lab's everything-on run free of
// restarts: renderConfig hands the baked bytes back unchanged and the
// applier finds nothing to swap.
type gwConfig struct {
	// apiVIP adds the API VIP's port_forwards block, together with the
	// port_forward_l3mdev_accept the same-host backend needs: the
	// backend socket sits in the default VRF while the VIP traffic
	// ingresses vrf-provider.
	apiVIP bool

	// portForwardOnly unsets both OVN remotes, which is what puts the
	// agent into port-forward-only mode (validateMode) — it then skips
	// every OVN-dependent codepath and runs as a standalone VIP service.
	// It only makes sense together with apiVIP: without port_forwards
	// there would be nothing left to do, and the agent refuses to start.
	portForwardOnly bool

	// hairpinVIP adds a second port_forwards block, for the VIP the
	// same-node hairpin-vip probe measures. It is only ever set together
	// with apiVIP: its backend is the very responder startAPIBackend
	// starts on API-VIP gateways, so on its own it would be a DNAT rule
	// with nothing behind it.
	hairpinVIP bool

	drainOnShutdown   bool
	cleanupOnShutdown bool
	networkCIDRs      []string
	reconcileInterval string
}

// empty reports whether the overlay changes anything at all.
func (c gwConfig) empty() bool {
	return !c.apiVIP && !c.hairpinVIP && !c.portForwardOnly && !c.drainOnShutdown &&
		!c.cleanupOnShutdown && len(c.networkCIDRs) == 0 && c.reconcileInterval == ""
}

// profile is one named chaos configuration: the topology layers the start
// state puts up, the agent configuration each gateway runs, and what the
// run measures.
type profile struct {
	name        string
	description string

	// The start-state layers. Everything the bootstrap baseline itself
	// seeds (the HA router, FIP 192.0.2.10, the vm1 workload) is always
	// there — a profile only decides what is layered on top.
	hairpin      bool // hairpin.sh's second FIP and its vm2 responder
	vlans        bool // multi-vlan.sh's two VLAN provider networks
	ovnLB        bool // pf-external.sh's OVN Load_Balancer VIP
	crossChassis bool // cross-chassis-fip.sh's second flat router on gateway-2 and its vm3 responder

	// gateways carries the per-gateway agent-config overlay. A gateway
	// absent from the map runs the baked lab config unchanged.
	gateways map[string]gwConfig

	probes []probeTarget
}

// gwConfig returns the overlay for one gateway — the zero value when the
// profile leaves it on the baked config.
func (p *profile) gwConfig(gw string) gwConfig { return p.gateways[gw] }

// apiVIPGateways names the gateways whose configuration carries the API
// VIP. They are the ones that need a responder behind it (startAPIBackend)
// and the only ones the masquerade flip means anything on.
func (p *profile) apiVIPGateways() []string {
	var gws []string
	for _, gw := range gatewayNames() {
		if p.gateways[gw].apiVIP {
			gws = append(gws, gw)
		}
	}
	return gws
}

// layers renders the profile's start topology for the journal.
func (p *profile) layers() string {
	on := []string{"bootstrap baseline"}
	if p.hairpin {
		on = append(on, "hairpin")
	}
	if p.vlans {
		on = append(on, "multi-vlan")
	}
	if p.ovnLB {
		on = append(on, "port-forward")
	}
	return strings.Join(on, " + ")
}

// everyGateway is the common case: one overlay, applied to all three
// gateways.
func everyGateway(c gwConfig) map[string]gwConfig {
	all := make(map[string]gwConfig, len(gatewayNames()))
	for _, gw := range gatewayNames() {
		all[gw] = c
	}
	return all
}

// profiles is the profile registry, in the order `-profile` lists it.
// Unlike the action registry, the order is not part of the replay
// contract — the profile is picked by name, not drawn — but the curated
// set is: adding one is a deliberate act, not a combinatorial sweep.
func profiles() []*profile {
	return []*profile{
		{
			name:         defaultProfileName,
			description:  "every topology layer, every gateway on the baked lab config",
			hairpin:      true,
			vlans:        true,
			ovnLB:        true,
			crossChassis: true,
			probes:       defaultProbes,
		},
		{
			name:        "flat-minimal",
			description: "no VLAN, no DNAT, cleanup on shutdown",
			gateways:    everyGateway(gwConfig{cleanupOnShutdown: true}),
			probes:      []probeTarget{probeVM1},
		},
		{
			name:         "flat-dnat",
			description:  "no VLAN; the agent's own DNAT on an API VIP, alongside the OVN Load_Balancer VIP",
			hairpin:      true,
			ovnLB:        true,
			crossChassis: true,
			gateways:     everyGateway(gwConfig{apiVIP: true, hairpinVIP: true}),
			probes: []probeTarget{
				probeVM1, probeVM2, probeLBVIP, probeAPIVIP,
				probeHairpinFIP, probeHairpinVIP, probeCrossFIP,
			},
		},
		{
			name:         "vlan-no-dnat",
			description:  "VLAN provider networks, no DNAT of any kind",
			hairpin:      true,
			vlans:        true,
			crossChassis: true,
			probes: []probeTarget{
				probeVM1, probeVM2, probeVLAN101, probeVLAN102, probeHairpinFIP,
				probeCrossFIP,
			},
		},
		{
			// No OVN remotes at all: the agent skips every OVN codepath,
			// so no topology layer is worth putting up and the API VIP is
			// the only thing left to measure. network_cidr is mandatory
			// here — without OVN the provider CIDRs cannot be discovered,
			// so nothing would announce the VIP (and the masquerade flip
			// would be rejected outright).
			name:        "pf-only",
			description: "no OVN connection: the agent runs as a standalone VIP service",
			gateways: everyGateway(gwConfig{
				apiVIP:          true,
				portForwardOnly: true,
				networkCIDRs:    []string{"192.0.2.0/24"},
			}),
			probes: []probeTarget{probeAPIVIP},
		},
		{
			// What a rollout looks like from the inside: the gateways run
			// different configurations at the same time. The API VIP sits
			// on gateway-1 *and* gateway-2: a full-mode gateway only
			// announces its port-forward VIPs while it holds a locally
			// active router (the dormant gate, #206), and at start every
			// router is active on gateway-1, the highest-priority chassis
			// — pinned to gateway-2 alone the VIP would be dormant on
			// every gateway and the api-vip probe could never go green.
			// Carried by both, the active chassis announces it from the
			// start, and losing gateway-1 hands lr0 — and with it the
			// announce — to gateway-2.
			name:         "heterogeneous",
			description:  "each gateway on a different configuration, as mid-rollout",
			hairpin:      true,
			vlans:        true,
			ovnLB:        true,
			crossChassis: true,
			gateways: map[string]gwConfig{
				"gateway-1": {apiVIP: true, hairpinVIP: true},
				"gateway-2": {apiVIP: true, hairpinVIP: true, drainOnShutdown: true},
				"gateway-3": {
					networkCIDRs:      explicitCIDRs,
					reconcileInterval: "15s",
					cleanupOnShutdown: true,
				},
			},
			probes: append(append([]probeTarget{}, defaultProbes...), probeAPIVIP, probeHairpinVIP),
		},
	}
}

// profileByName resolves the -profile flag.
func profileByName(name string) (*profile, error) {
	known := make([]string, 0, len(profiles()))
	for _, p := range profiles() {
		if p.name == name {
			return p, nil
		}
		known = append(known, p.name)
	}
	return nil, fmt.Errorf("unknown profile %q (known: %s)", name, strings.Join(known, ", "))
}

// renderConfig layers one gateway's overlay over the baked lab config.
//
// An empty overlay returns the base bytes verbatim, and that byte
// identity is load-bearing: the applier compares what it rendered against
// the file already on the gateway to decide whether it has to swap the
// config and restart the node at all.
func renderConfig(base []byte, c gwConfig, mgmtIP string) ([]byte, error) {
	if c.empty() {
		return base, nil
	}
	doc, err := parseConfig(base)
	if err != nil {
		return nil, err
	}
	applyOverlay(doc, c, mgmtIP)
	return marshalConfig(doc)
}

// applyOverlay writes the overlay's keys into a parsed config document,
// in place. Every write is key-level rather than an append, because
// yaml.v3 — which the agent itself parses the file with — rejects a
// document that carries the same mapping key twice.
func applyOverlay(doc map[string]any, c gwConfig, mgmtIP string) {
	if c.portForwardOnly {
		delete(doc, "ovn_sb_remote")
		delete(doc, "ovn_nb_remote")
	}
	if c.drainOnShutdown {
		doc["drain_on_shutdown"] = true
	}
	if c.cleanupOnShutdown {
		doc["cleanup_on_shutdown"] = true
	}
	if len(c.networkCIDRs) > 0 {
		doc["network_cidr"] = anySlice(c.networkCIDRs)
	}
	if c.reconcileInterval != "" {
		doc["reconcile_interval"] = c.reconcileInterval
	}
	if c.apiVIP || c.hairpinVIP {
		doc["port_forward_l3mdev_accept"] = true
		var vips []any
		if c.apiVIP {
			vips = append(vips, apiVIPBlock(mgmtIP))
		}
		if c.hairpinVIP {
			vips = append(vips, hairpinVIPBlock(mgmtIP))
		}
		doc["port_forwards"] = vips
	}
}

// apiVIPBlock is the API VIP's port_forwards entry: the agent owns the
// VIP address, and one TCP rule forwards it to the responder on the
// gateway's own management address.
//
// masquerade is off on purpose: the backend runs on the same host, and
// SNAT-ing traffic to a same-host backend breaks the reply path (the
// OUTPUT chains handle the replies, see docs/explanation/port-forwarding.md).
// hairpin_masquerade is spelled out rather than left to its default
// because the masquerade flip toggles it: with the key present, toggling
// it twice puts the gateway back on exactly the profile's configuration.
func apiVIPBlock(mgmtIP string) map[string]any {
	return map[string]any{
		"vip":                apiVIPAddr,
		"manage_vip":         true,
		"masquerade":         false,
		"hairpin_masquerade": false,
		"rules": []any{map[string]any{
			"proto":     "tcp",
			"port":      apiVIPPort,
			"dest_addr": mgmtIP,
			"dest_port": apiVIPPort,
		}},
	}
}

// hairpinVIPBlock is the hairpin VIP's port_forwards entry. It points at
// the same responder as the API VIP, on the same port, and differs only
// in the VIP a client asks for — so what the hairpin-vip probe measures
// is the path, not the backend.
//
// The path it measures: vm1 to OVN's egress SNAT (source FIP_A) at the
// master, lr0's default route out cr-lr0-public onto br-ex, the MAC-tweak
// flow into the kernel, nftables prerouting DNAT to the gateway's own
// management address, the host-local backend, conntrack reversal on
// OUTPUT, and the reply steered back into OVN by the agent's per-FIP
// policy rule. masquerade stays off for the reason apiVIPBlock gives:
// SNAT-ing to a same-host backend breaks that reply path. The masquerade
// postrouting chain is therefore not on this path — a host-local backend
// is INPUT-bound — and stays covered by pf-hairpin.sh alone.
func hairpinVIPBlock(mgmtIP string) map[string]any {
	return map[string]any{
		"vip":                hairpinVIPAddr,
		"manage_vip":         true,
		"masquerade":         false,
		"hairpin_masquerade": false,
		"rules": []any{map[string]any{
			"proto": "tcp",
			"port":  hairpinVIPPort,
			// The responder listens on the API VIP's port; only the VIP
			// a client asks for differs.
			"dest_addr": mgmtIP,
			"dest_port": apiVIPPort,
		}},
	}
}

// flipVIPBlock is the VIP the pf-rule-toggle flip adds. It points at the
// same responder as the API VIP but on a port of its own, so a gateway
// that carries it without an API backend behind it is a dead DNAT rule —
// which is exactly the churn the flip is there to produce, and why
// nothing probes it.
func flipVIPBlock(mgmtIP string) map[string]any {
	return map[string]any{
		"vip":        flipVIPAddr,
		"manage_vip": true,
		"masquerade": false,
		"rules": []any{map[string]any{
			"proto":     "tcp",
			"port":      flipVIPPort,
			"dest_addr": mgmtIP,
			"dest_port": apiVIPPort,
		}},
	}
}

func parseConfig(raw []byte) (map[string]any, error) {
	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse agent config: %w", err)
	}
	return doc, nil
}

func marshalConfig(doc map[string]any) ([]byte, error) {
	raw, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("render agent config: %w", err)
	}
	return raw, nil
}

// anySlice widens a string list into what a YAML round-trip produces, so
// a rendered document and a re-parsed one carry the same Go types — the
// flips read and rewrite these values.
func anySlice(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// stringsOf narrows a YAML list (or a scalar) back to a string slice.
func stringsOf(value any) []string {
	switch v := value.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{v}
	default:
		return nil
	}
}
