# Set up port forwarding (DNAT)

This guide walks through configuring the agent to forward traffic from anycast
VIP addresses to internal backends. For the conceptual model — connmarks,
fwmarks, VRF crossing, packet-flow diagrams — see
[Port forwarding (DNAT)](../explanation/port-forwarding).

## Prerequisites

- **`nft` binary** must be in `PATH` (the agent shells out to `nft -f -` for
  atomic ruleset application).
- **IPv4 only** — VIP and backend addresses must be IPv4; IPv6 is not
  supported for port forwarding.
- **`veth_leak_enabled: true`** (default) — port forwarding requires the veth
  pair for the return path.
- **IP forwarding** on the veth interfaces — enabled automatically by the
  agent at startup.

Port forwarding works whether or not the node is an OVN gateway. A node that
should *only* expose VIPs can run with no OVN remotes configured at all — see
[port-forward-only mode](configuration#port-forward-only-mode).

## Example configuration

```yaml
port_forward_dev: "loopback1"   # VIP addresses go on this interface in vrf-provider
port_forward_table_id: 201      # dedicated routing table for DNAT return traffic
# port_forward_ct_zone: 64000   # conntrack zone (default 64000, must not collide with OVN zones)
# port_forward_l3mdev_accept: false  # set true if same-host backends are in a different VRF than the VIP

port_forwards:
  - vip: "198.51.100.10"
    manage_vip: true             # agent adds 198.51.100.10/32 to loopback1
    masquerade: true             # VIP-level default: rules inherit this unless overridden
    rules:
      # Local backend (same host): override masquerade to false.
      # Reply is handled by output_ctzone/output_fwmark chains.
      - proto: udp
        port: 53
        dest_addr: "10.0.0.200"
        dest_port: 1053
        masquerade: false
      - proto: tcp
        port: 53
        dest_addr: "10.0.0.200"
        dest_port: 1053
        masquerade: false
      # Remote backend (different host): inherits masquerade: true from VIP.
      # SNAT ensures replies return to this node for reverse NAT.
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"
      # Multiple backends with sticky hashing:
      - proto: udp
        port: 5353
        dest_addrs:
          - "10.0.0.200"
          - "10.0.0.201"
          - "10.0.0.202"
        dest_port: 1053

  # VIP with hairpin_masquerade: fixes connections from FIPs on the same node.
  # External clients are NOT masqueraded (client IP preserved end-to-end).
  - vip: "198.51.100.20"
    manage_vip: true
    hairpin_masquerade: true     # SNAT only for source IPs within provider networks
    rules:
      - proto: tcp
        port: 80
        dest_addr: "10.0.0.100"
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"

  # VIP that must be reachable from both FIP-equipped VMs and instances
  # behind a router without a FIP. hairpin_masquerade covers the former,
  # router_masquerade the latter — both rules coexist in postrouting_snat
  # and leave every other external client IP untouched.
  - vip: "198.51.100.30"
    manage_vip: true
    hairpin_masquerade: true     # FIPs on the same node
    router_masquerade: true      # instances behind a router (no FIP)
    rules:
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"
```

## VIP address management

Each VIP can optionally be managed by the agent (`manage_vip: true`). When
enabled, the agent adds the VIP as a `/32` address on the configured loopback
interface (default: `loopback1`) inside `vrf-provider`. That address is the
VIP's BGP announce path: its **connected route** is what FRR redistributes to
the external fabric. The agent does *not* write an FRR static route for the VIP
— a static to the same `/32` would be permanently shadowed by the connected
route (a local address always wins over a static to the same prefix) and never
advertised.

For the announcement to leave the box, the gateway's BGP configuration must
redistribute connected routes through the `ANNOUNCED-NETWORKS` prefix-list, via
a route-map:

```
router bgp <asn> vrf vrf-provider
 address-family ipv4 unicast
  redistribute connected route-map ANNOUNCE-CONNECTED
 exit-address-family
!
route-map ANNOUNCE-CONNECTED permit 10
 match ip address prefix-list ANNOUNCED-NETWORKS
```

The route-map form is deliberate. An undefined prefix-list inside a route-map
`match` is a *no-match* that falls through to the route-map's terminal deny, so
a standby that has emptied `ANNOUNCED-NETWORKS` exports nothing via connected —
whereas a bare `redistribute connected`, or only a neighbor `prefix-list … out`
naming an undefined list, would treat the missing list as permit-all and leak
the underlay `/30`s.

The agent itself keeps the prefix-list entries the VIPs need: alongside the
per-network `permit <network> ge 32 le 32` entries, every announceable VIP
gets its own exact `permit <vip>/32` entry. The VIP entry is load-bearing, not
redundancy — a VIP need not lie inside any network the gateway currently
hosts (a flat-range VIP on a chassis that hosts only VLAN routers has no
covering network entry), and without its own entry the connected route would
be silently filtered while the address, DNAT, and kernel route all look
healthy.

On a full-mode gateway that hosts no local routers the agent **removes** the
managed VIP address (see the dormancy section in the port-forwarding
explanation). The reasoning: on such a standby the agent has emptied
`ANNOUNCED-NETWORKS`, so a present address must not be exportable — withholding
the address is what keeps the standby from announcing a VIP it does not own.

When `manage_vip: false`, the VIP address must already exist on the interface
(e.g. configured statically or by another tool).

## Sticky load balancing (multi-backend)

When a rule specifies multiple backends via `dest_addrs`, the agent generates
nftables rules using `jhash ip saddr` (Jenkins hash on the client's source IP)
to consistently map the same client to the same backend:

```
ip daddr 198.51.100.10 udp dport 53 dnat to jhash ip saddr mod 3 map { \
    0 : 10.0.0.200:1053, \
    1 : 10.0.0.201:1053, \
    2 : 10.0.0.202:1053  \
}
```

**Properties:**

- **Sticky**: the same client IP always reaches the same backend
  (deterministic hash).
- **Distributed**: different clients are spread evenly across all backends.
- **Conntrack-aware**: within an established conntrack entry, replies
  naturally stay on the same backend; `jhash` ensures that *new* connections
  from the same client also land on the same backend.
- **NAT-friendly**: clients behind the same NAT gateway (same source IP) share
  a backend, which is typically the desired behavior for DNS and similar
  services.

**Limitations:**

- Not a consistent hash (like Maglev or ketama): when a backend is added or
  removed, `mod N` changes and approximately `(N-1)/N` of clients may be
  remapped. For DNS stickiness this is acceptable in practice.
- `dest_addr` (single) and `dest_addrs` (list) are mutually exclusive per
  rule. Use `dest_addr` for single-backend rules and `dest_addrs` for
  multi-backend.
- Maximum 256 backends per rule.

## Hairpin NAT

Two flavors of source-selective masquerade are available to fix hairpin NAT,
depending on how the local client reaches the VIP. They can be combined on
the same VIP — the resulting rules coexist in `postrouting_snat`.

### Case 1: instance with a FIP on the same node (`hairpin_masquerade`)

**The problem:** a VM with a Floating IP (FIP) in the provider network (e.g.
`5.182.234.153`) tries to reach a port-forwarded VIP (e.g. `194.93.78.239`)
on the same node. ICMP to the VIP succeeds because the VIP address is local
(`loopback1`) and the kernel responds directly — DNAT is never involved. TCP
connections time out because:

1. The VM's packet is DNAT'd:
   `src=5.182.234.153 dst=194.93.78.239:80` → `dst=backend_ip:80`.
2. The backend replies to `5.182.234.153` directly — but without SNAT the
   reply may not return through this node (asymmetric routing), so conntrack
   never sees it and the reverse DNAT fails silently.

**The fix:** enable `hairpin_masquerade: true` on the VIP. The agent adds a
source-selective SNAT rule that masquerades only traffic from provider
networks:

```
# nftables postrouting_snat chain (generated when hairpin_masquerade: true)
ip saddr 5.182.234.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade
```

With this rule active:

1. The backend receives the packet with `src=<node-control-plane-IP>` instead
   of the FIP.
2. The backend replies to the node's control-plane IP (always reachable).
3. Conntrack reverses both SNAT and DNAT: the VM receives the reply from
   `194.93.78.239`.

External clients (source outside provider networks) are unaffected — their
IPs are still preserved end-to-end.

**Difference from `masquerade: true`:** the VIP-level `masquerade` masquerades
ALL traffic. Hairpin masquerade only masquerades source IPs within the
provider networks, leaving external client IPs intact.

**Note:** hairpin masquerade rules require the provider networks to be known.
On the very first startup (before OVN discovery completes), the rules are
absent. They are installed automatically on the first reconciliation cycle
once OVN reports the provider network CIDRs.

**E2E coverage:** the containerlab harness has an explicit scenario for
this flag — see
[`pf-hairpin.sh`](../contributing/e2e-tests.md#running-locally) (issue
[#110](https://github.com/osism/ovn-network-agent/issues/110)). It
drives a co-located FIP workload against a port-forwarded VIP twice
on the same lab: once with `hairpin_masquerade: false` (the TCP probe
**must** time out, because the reply bypasses the chassis conntrack)
and once with `hairpin_masquerade: true` (the probe **must** complete
within the reconcile budget). A regression that accidentally fixes
the negative case turns CI red.

### Case 2: instance behind a router without a FIP (`router_masquerade`)

**The problem:** an instance without its own FIP connects to a port-forwarded
VIP through an OVN router. The router applies SNAT, so the packet that hits
the VIP carries the **router's external IP** as source instead of the instance
address. That external IP is OVN-managed — meaning the backend's reply enters
OVN's pipeline directly, **bypassing this node's conntrack**. The reverse
DNAT never fires, the instance receives a reply from the backend IP instead
of the VIP, and the connection fails.

`hairpin_masquerade` does not fix this cleanly: matching the full provider
CIDR would also rewrite every unrelated external client living in the same
subnet.

**The fix:** enable `router_masquerade: true` on the VIP. The agent
dynamically discovers router SNAT external IPs from OVN NB (rows where
`nat.type=snat` on locally-active routers) and emits a source-selective SNAT
rule that targets **only those specific addresses**:

```
# Single SNAT IP
ip saddr 203.0.113.50 ct original daddr 194.93.78.239 ct status dnat masquerade

# Multiple SNAT IPs (anonymous set)
ip saddr { 203.0.113.50, 203.0.113.51 } ct original daddr 194.93.78.239 ct status dnat masquerade
```

With this rule active the backend sees `src=<node-control-plane-IP>`, replies
through this node, and conntrack reverses both SNAT and DNAT — the instance
receives the reply from the VIP exactly as it expects.

**Difference from `hairpin_masquerade`:** hairpin masquerade matches a
provider-CIDR prefix and therefore also rewrites unrelated external clients
that share the subnet. Router masquerade matches the literal set of router
SNAT IPs surfaced by OVN, leaving every other client untouched.

**Note:** router masquerade rules require at least one SNAT IP to be known.
If the agent starts before OVN has reported any SNAT entry, the rule (and
the entire `postrouting_snat` chain if it would otherwise be empty) is
omitted to prevent accidental masquerade during startup. The rule is
installed on the first reconciliation cycle that delivers SNAT IP data.
