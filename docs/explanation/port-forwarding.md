# Port forwarding (DNAT)

## Background: the problem

Some services running on the gateway nodes themselves (not inside VMs) need
to be reachable via the same anycast VIP addresses that BGP announces to the
external fabric. Examples include DNS resolvers, monitoring collectors, or
API proxies that run directly on the network nodes.

These services listen on internal addresses (e.g. `10.0.0.200:1053`) but
need to be reachable from outside via a public VIP (e.g.
`198.51.100.10:53`). A simple `iptables DNAT` rule handles the destination
translation, but the **return path** is the hard part: the backend's reply
has a source address (`10.0.0.200`) that doesn't match any provider network
— so it would be routed via the default VRF instead of through
`vrf-provider` where BGP can deliver it to the external client.

The naive fix would be SNAT/masquerade, which rewrites the source to the VIP
address. But this **destroys the client IP** — the backend sees the VIP as
the source instead of the real client, breaking logging, rate limiting,
ACLs, and any protocol that depends on client identity.

## Solution: connmark-based return routing

The agent uses nftables with connection tracking marks (connmarks) to steer
DNAT return traffic through the veth pair into `vrf-provider` — without
masquerade, preserving the original client IP end-to-end. For remote
backends (different network segment, reply must return to this node),
per-rule masquerade can be enabled via `masquerade: true` on individual
rules or inherited from the VIP level. Local backends (same host) must NOT
be masqueraded — their replies are handled by dedicated OUTPUT chains
instead.

The mechanism works in six stages (the first four handle remote backends,
stages 5-6 add same-host backend support):

**1. DNAT (prerouting)** — Translates the destination for incoming traffic:

```
# Single backend:
ip daddr 198.51.100.10 tcp dport 53 dnat to 10.0.0.200:1053

# Multiple backends (sticky source-IP hashing):
ip daddr 198.51.100.10 udp dport 53 dnat to jhash ip saddr mod 3 map { \
    0 : 10.0.0.200:1053, 1 : 10.0.0.201:1053, 2 : 10.0.0.202:1053 }
```

Traffic arriving at the VIP is rewritten to the internal backend address.
If `dest_port` is configured, port translation also occurs (e.g. public port
53 → backend port 1053). When multiple backends are configured via
`dest_addrs`, a Jenkins hash on the client source IP distributes traffic
with sticky affinity (see
[Sticky load balancing](../guides/port-forwarding#sticky-load-balancing-multi-backend)).

**2. Conntrack zone assignment (prerouting_ctzone, raw priority)** — Assigns
a shared conntrack zone before conntrack processing. This is critical
because DNAT'd traffic crosses VRF boundaries (original enters on the
provider VRF, reply enters on the default VRF). Without a shared zone,
conntrack cannot correlate them and reverse NAT fails silently. The zone
number defaults to 64000 (configurable via `port_forward_ct_zone`) to avoid
collisions with OVN/OVS conntrack zones:

```
# Original direction: client → VIP:port
ip daddr 198.51.100.10 tcp dport 53 ct zone set 64000

# Reply direction: backend:port → client
ip saddr 10.0.0.200 tcp sport 1053 ct zone set 64000
```

**3. Fwmark tagging (prerouting_fwmark, filter priority)** — Marks DNAT'd
packets with direction-specific fwmarks for policy routing. Two marks steer
traffic into different routing tables:

```
# Original direction (client→backend): fwmark 0x100 → lookup main
# Escapes the VRF so DNAT'd traffic reaches the backend via the default VRF
ct direction original ct status dnat ct original daddr { 198.51.100.10 } meta mark set 0x100

# Reply direction (backend→client): fwmark 0x200 → lookup table 201
# Routes reply through veth pair back into vrf-provider for BGP delivery
ct direction reply ct status dnat ct original daddr { 198.51.100.10 } meta mark set 0x200
```

**4. Same-host conntrack zone (output_ctzone, raw priority)** — Mirrors
`prerouting_ctzone` for the OUTPUT hook. When a DNAT backend runs on the
same host, the packet is delivered locally (INPUT chain, not FORWARD). The
reply from the local process (e.g. docker-proxy) originates in OUTPUT, not
PREROUTING. Without this chain, conntrack cannot find the DNAT entry (wrong
zone) and reverse NAT fails:

```
# Same rules as prerouting_ctzone, but in the output hook:
ip daddr 198.51.100.10 tcp dport 53 ct zone set 64000
ip saddr 10.0.0.200 tcp sport 1053 ct zone set 64000
```

**5. Same-host fwmark (output_fwmark, type route)** — Mirrors the
reply-direction mark from `prerouting_fwmark` for locally generated
replies. Uses `type route` so the mark change triggers a routing
re-evaluation, steering the reply through the veth pair back into
`vrf-provider`:

```
ct direction reply ct status dnat ct original daddr { 198.51.100.10 } meta mark set 0x200
```

**6. Policy routing** — Two fwmark-based `ip rule` entries steer DNAT'd
traffic bidirectionally:

```
ip rule: fwmark 0x100 → lookup main       (priority 150, original direction)
ip rule: fwmark 0x200 → lookup table 201   (priority 151, reply direction)
table 201: default via 169.254.0.2 dev veth-default
```

The forward rule escapes the VRF so packets reach the backend. The reply
rule sends the backend's response back through the veth pair into
`vrf-provider`, where FRR/BGP delivers it to the external client. The
client IP is preserved throughout — no masquerade anywhere in the path.

A `postrouting_fwmark_clear` chain clears the `0x200` fwmark before packets
cross the veth pair, preventing a routing loop where the mark would match
again inside the provider VRF.

**7. Per-rule masquerade (postrouting_snat, optional)** — When
`masquerade: true` is set on a rule (or inherited from the VIP), SNAT is
applied to traffic going to that specific backend. The masquerade rule
matches on the post-DNAT destination address, so only remote backends are
affected:

```
# Only masquerades traffic to remote backend 10.0.0.100, not to local backends
ip daddr 10.0.0.100 tcp dport 443 ct status dnat masquerade
```

This per-backend granularity is essential when a VIP has both local and
remote backends: local backends must NOT be masqueraded (their replies are
handled by the output chains), while remote backends MUST be masqueraded so
replies return to this node for reverse NAT.

**8. Hairpin masquerade (postrouting_snat, optional)** — When
`hairpin_masquerade: true` is set on a VIP, SNAT is applied **only to
traffic whose source is within a provider network**. This solves the
hairpin NAT problem: a VM with a Floating IP (FIP) on the same node that
connects to the VIP gets its source address masqueraded, so the backend
always replies through this node and conntrack can perform the reverse
DNAT. Traffic from external clients (source outside provider networks) is
never masqueraded — their client IPs are preserved end-to-end.

The hairpin masquerade rule uses `ct original daddr` to match the pre-DNAT
destination (the VIP), ensuring only traffic belonging to connections that
were originally destined for this specific VIP is affected:

```
# Traffic from provider net → VIP, DNAT'd: masquerade so backend replies here
ip saddr 5.182.234.0/24 ct original daddr 194.93.78.239 ct status dnat masquerade
```

Unlike the VIP-level `masquerade: true` (which masquerades ALL traffic),
hairpin masquerade is source-selective. It can be combined with per-rule
`masquerade` on the same VIP — both rules coexist in `postrouting_snat`.
The rules are only generated when provider networks are known; if the agent
starts before OVN has delivered network discovery, they appear on the first
reconciliation cycle.

**9. Router masquerade (postrouting_snat, optional)** — When
`router_masquerade: true` is set on a VIP, SNAT is applied **only to
traffic whose source matches a known router SNAT external IP** (discovered
dynamically from OVN NB `nat.type=snat` entries on locally-active routers).
This solves the hairpin NAT problem for instances **behind a router that do
not have a Floating IP**: the router applies SNAT before forwarding, so the
source IP arriving at the VIP is the router's OVN-managed external address.
Without masquerade, the backend's reply enters OVN's pipeline directly —
bypassing this node's conntrack — and the reverse DNAT never fires, so the
instance receives a packet from the backend IP instead of the VIP and the
connection breaks.

With a single known SNAT IP the rule is emitted as a literal address match;
with multiple IPs nft's anonymous set syntax is used so all known router
sources are covered by one rule:

```
# Single SNAT IP
ip saddr 203.0.113.50 ct original daddr 194.93.78.239 ct status dnat masquerade

# Multiple SNAT IPs
ip saddr { 203.0.113.50, 203.0.113.51 } ct original daddr 194.93.78.239 ct status dnat masquerade
```

Unlike `hairpin_masquerade` (which uses the full provider CIDR and would
also masquerade unrelated external clients that happen to live in the same
subnet), `router_masquerade` is **more surgical**: only the specific router
SNAT IPs are rewritten, leaving every other external client IP fully
preserved end-to-end. `router_masquerade` and `hairpin_masquerade` can be
set together on the same VIP — both rules coexist in `postrouting_snat`,
covering FIP-sourced and router-SNAT'd traffic simultaneously. The rule is
only emitted once OVN has reported at least one SNAT IP; on a cold start
(before the first reconcile delivers SNAT data) the chain is omitted
entirely to prevent accidental masquerade.

## Why conntrack-based fwmark instead of simpler alternatives?

| Approach | Client IP preserved? | Problem |
|----------|---------------------|---------|
| SNAT/masquerade (global) | No | Backend sees VIP as source, not the real client |
| Source-based routing (`ip rule from <backend>`) | Yes | Catches **all** traffic from the backend, not just DNAT replies — breaks normal connectivity |
| Conntrack + fwmark | Yes | Only marks packets belonging to DNAT'd connections — surgical, no side effects |
| Conntrack + fwmark + per-rule masquerade | Depends | Best of both: client IP preserved for local backends, masquerade only where needed (remote backends) |
| Conntrack + fwmark + hairpin masquerade | Depends | Client IP preserved for external clients; source-selective SNAT fixes hairpin for FIPs on the same node |
| Conntrack + fwmark + router masquerade | Depends | Client IP preserved for external clients; source-selective SNAT against known router SNAT IPs fixes hairpin for instances behind a router without a FIP |

The conntrack-based approach selectively routes just the DNAT return
traffic without affecting any other traffic from the backend. It uses
`ct status dnat` and `ct direction` to identify packets belonging to
DNAT'd connections and assigns direction-specific fwmarks for policy
routing. Per-rule masquerade adds surgical SNAT only for remote backends
that need it, while local backends (same host) use the OUTPUT chains for
return routing with the original client IP preserved. Hairpin masquerade
adds a further refinement: source-selective SNAT only for provider-network
traffic, solving the asymmetric routing that occurs when a FIP on the same
node connects to the VIP. Router masquerade is the no-FIP companion: it
targets the specific router SNAT IPs surfaced by OVN, so only those
addresses are rewritten and every other external client remains untouched.

## The `forward_veth_guard` chain

The veth pair between default VRF and `vrf-provider` is a controlled leak —
only specific traffic should traverse it backwards (from default VRF into
`vrf-provider`). Without a guard, any packet in the default VRF that happens
to be routed via the veth pair could leak into `vrf-provider`.

The `forward_veth_guard` nftables chain enforces a whitelist on traffic
exiting through `veth-default`:

```
chain forward_veth_guard {
    type filter hook forward priority filter; policy accept;

    # Allow legitimate veth-leak return traffic (SNAT replies from provider networks)
    oifname "veth-default" ip saddr { 192.0.2.0/24, 198.51.100.0/24 } accept

    # Allow DNAT reply traffic (identified by fwmark 0x200 from prerouting_fwmark)
    oifname "veth-default" meta mark 0x200 accept

    # Drop everything else — prevents unintended traffic from leaking into vrf-provider
    oifname "veth-default" drop
}
```

The provider network CIDRs in the first rule are populated dynamically from
the same auto-discovered (or manually configured) networks used by the rest
of the agent. They are updated on every reconciliation cycle.

## Packet flow: DNAT forward and return path

### Forward path (external client → backend)

```
 External client
 src=203.0.113.50  dst=198.51.100.10:53
         │
         │ BGP route: 198.51.100.10/32
         ▼
 ┌──────────────────────────────────────────────────────────────┐
 │                       vrf-provider                           │
 │                                                              │
 │  lo: 198.51.100.10/32 (VIP, managed by agent)                │
 │  FRR/BGP announces this /32 to external fabric               │
 │                                                              │
 │  Packet arrives via BGP peering                              │
 │  /32 route: 198.51.100.10 via 169.254.0.1                    │
 │                                                              │
 │  veth-provider (169.254.0.2/30)                              │
 └──────────────┬───────────────────────────────────────────────┘
                │ veth pair
 ┌──────────────▼───────────────────────────────────────────────┐
 │  veth-default (169.254.0.1/30)        Default VRF            │
 │                                                              │
 │  nft prerouting_ctzone:                                      │
 │  ├─ ct zone set 64000 (shared zone for cross-VRF conntrack)  │
 │  nft prerouting_dnat:                                        │
 │  ├─ DNAT 198.51.100.10:53 → 10.0.0.200:1053                  │
 │  nft prerouting_fwmark:                                      │
 │  ├─ ct direction original + ct status dnat → fwmark 0x100    │
 │  ip rule: fwmark 0x100 → lookup main (escapes VRF)           │
 │                                                              │
 │  Kernel delivers to local backend process                    │
 │  └─ dst=10.0.0.200:1053 (client IP 203.0.113.50 preserved)   │
 └──────────────────────────────────────────────────────────────┘
```

### Return path — remote backend (backend → external client)

When the backend is on a different host, the reply arrives via the network
and enters PREROUTING:

```
 ┌──────────────────────────────────────────────────────────────┐
 │                        Default VRF                           │
 │                                                              │
 │  Remote backend replies (arrives via network):               │
 │  └─ src=10.0.0.200:1053  dst=203.0.113.50                    │
 │  nft prerouting_fwmark (reply direction):                    │
 │  ├─ ct direction reply + ct status dnat → fwmark 0x200       │
 │  Conntrack un-DNATs source address:                          │
 │  ├─ src becomes 198.51.100.10:53                             │
 │  ip rule: fwmark 0x200 → lookup table 201                    │
 │  └─ table 201: default via 169.254.0.2 dev veth-default      │
 │  nft postrouting_fwmark_clear:                               │
 │  ├─ clears fwmark 0x200 before crossing veth (prevents loop) │
 │                                                              │
 │  veth-default (169.254.0.1/30)                               │
 └──────────────┬───────────────────────────────────────────────┘
                │ veth pair
 ┌──────────────▼───────────────────────────────────────────────┐
 │                       vrf-provider                           │
 │  veth-provider (169.254.0.2/30)                              │
 │                                                              │
 │  FRR/BGP delivers reply to external client                   │
 │  └─ src=198.51.100.10:53  dst=203.0.113.50                   │
 └──────────────┬───────────────────────────────────────────────┘
                │
                ▼
         External client
         (client IP preserved end-to-end)
```

### Return path — same-host backend (local process → external client)

When the backend runs on the same node, the forward packet is delivered
locally (INPUT chain), and the reply originates from the OUTPUT hook. The
`output_ctzone` and `output_fwmark` chains handle this path:

```
 ┌──────────────────────────────────────────────────────────────┐
 │                        Default VRF                           │
 │                                                              │
 │  Local backend replies (OUTPUT hook, not PREROUTING):        │
 │  └─ src=10.0.0.200:1053  dst=203.0.113.50                    │
 │  nft output_ctzone (raw priority):                           │
 │  ├─ ct zone set 64000 (same zone as prerouting_ctzone)       │
 │  Conntrack finds DNAT entry in zone 64000, un-DNATs:         │
 │  ├─ src becomes 198.51.100.10:53                             │
 │  nft output_fwmark (type route → triggers re-routing):       │
 │  ├─ ct direction reply + ct status dnat → fwmark 0x200       │
 │  ip rule: fwmark 0x200 → lookup table 201                    │
 │  └─ table 201: default via 169.254.0.2 dev veth-default      │
 │  nft postrouting_fwmark_clear:                               │
 │  ├─ clears fwmark 0x200 before crossing veth (prevents loop) │
 │                                                              │
 │  veth-default (169.254.0.1/30)                               │
 └──────────────┬───────────────────────────────────────────────┘
                │ veth pair
 ┌──────────────▼───────────────────────────────────────────────┐
 │                       vrf-provider                           │
 │  veth-provider (169.254.0.2/30)                              │
 │                                                              │
 │  FRR/BGP delivers reply to external client                   │
 │  └─ src=198.51.100.10:53  dst=203.0.113.50                   │
 └──────────────┬───────────────────────────────────────────────┘
                │
                ▼
         External client
         (client IP preserved end-to-end)
```

## Running as a standalone VIP service (port-forward-only mode)

Port forwarding does not depend on OVN. The DNAT rules, the VIP addresses,
and the connmark-based return routing are all managed independently of any
OVN gateway state — the reconcile loop asserts them regardless of local
router presence (see [Dormant VIPs](#dormant-vips-on-a-gateway-without-local-routers)
for the one part that is gated: the routes that announce the VIPs).

That makes it useful to run the agent as a *pure* VIP service on a node
that is **not** an OVN gateway chassis at all: a node hosting DNS
resolvers, monitoring collectors, or API proxies that should be reachable
via anycast VIPs, but that has no Floating IPs, no chassisredirect ports,
and no gateway routing to maintain.

When `port_forwards` is configured but both OVN remotes (`ovn_sb_remote`
and `ovn_nb_remote`) are left unset, the agent starts in
**port-forward-only mode**. It manages only the VIPs and never opens an OVN
connection — removing an operational dependency and the spurious
`failed to connect to OVN, retrying` logging that an unreachable OVN
endpoint would otherwise produce.

What still runs in this mode:

- `SetupPortForward` / `ReconcilePortForward` / `TeardownPortForward` — the
  DNAT rules, VIP addresses, conntrack zones, and policy routing described
  above.
- The veth pair between the default VRF and `vrf-provider`, used by the
  DNAT return path.
- The managed VIP addresses on `port_forward_dev`, whose connected routes BGP
  redistributes to announce them (no FRR static route is written for a VIP — it
  would be shadowed by the connected route and never advertised).
- The periodic reconcile ticker, which re-asserts the VIP and DNAT state.

What is skipped, because it is OVN-specific:

- The OVN SB/NB connection and the entire OVN client lifecycle.
- Gateway routing, OVS hairpin/MAC-tweak flows, stale chassis cleanup,
  drain-on-shutdown, and FIP/SNAT-IP route management.
- Provider-bridge setup (`br-ex`). A pure VIP-service node need not have
  `br-ex`: no `<vip>/32` kernel route is installed on it, the VIP is
  reachable as a local address on `port_forward_dev`, and the connected route
  of that address carries the BGP announcement (via `redistribute connected`
  through the `ANNOUNCED-NETWORKS` route-map).

Because router SNAT IPs and provider CIDRs are normally discovered from
OVN, the two source-selective masquerade options that rely on them are
constrained in this mode (see
[Configure the agent](../guides/configuration#port-forward-only-mode)):
`router_masquerade` is rejected outright, and `hairpin_masquerade`
requires an explicit `network_cidr`.

## Dormant VIPs on a gateway without local routers

A VIP is only announceable on a node where the agent also maintains the FRR
prefix-list that permits it. On a gateway that hosts no locally active
router — a standby chassis, or one whose routers have failed over
elsewhere — reconcile takes the standby path and empties both that
prefix-list and the veth-leak network routes. A VIP announced there could
never be advertised.

Because a VIP's announce path is now the connected route of its address on
`port_forward_dev`, the only lever that model leaves is the address itself.
The agent therefore treats such a VIP as **dormant**: it **withholds the VIP
address** from `port_forward_dev` (so no connected route exists to export),
installs no `<vip>/32` kernel route, and reports the condition once at INFO
level:

```
port-forward VIPs are dormant on this node — no locally active routers,
so they cannot be advertised and their address is withheld from the
port-forward device
```

The DNAT plane is *not* withheld. The nftables rules, the conntrack zones,
and the policy routing are all still asserted, so a gateway that later gains
a local router only needs the address and the announce — the DNAT data path
is already in place. When that happens the agent logs `port-forward VIPs are
no longer dormant` and restores the address (and its kernel route) on the
next reconcile.

Port-forward-only mode never takes the standby path, so its VIPs are never
dormant — a pure VIP-service node serves them without any OVN state at all.

::: tip Why withhold the address rather than the route?
The connected route of a local address always wins over a static to the same
`/32`, so the agent no longer writes a VIP static at all — the connected
route is the single announce path. That leaves the address as the only thing
to withhold. Withholding it is also belt-and-suspenders with the BGP config:
`redistribute connected` runs through a route-map matching `ANNOUNCED-NETWORKS`,
and on a standby that list is empty, so even a briefly-present address would
not be exported. The earlier design installed an FRR static unconditionally,
which zebra never selected (the connected route shadowed it): not advertised,
but present. That held `ovn_network_agent_inactive_routes` at a non-zero value
— which the shipped `OVNNetworkAgentInactiveRoutes` alert fires on — and logged
`FRR static routes are configured but inactive` at ERROR level on every
reconcile, indefinitely.
:::
