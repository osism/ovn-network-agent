# Architecture

The agent has two complementary halves: a **control plane** that watches OVN
and decides what to do, and a **data plane** that delivers user traffic
between OVN's logical pipeline and the external BGP fabric. The reconcile
loop (see [How it works](how-it-works)) sits on top, repeatedly aligning the
two.

## Control plane

The agent monitors OVN databases and writes routing state into four
subsystems. On every change (or periodically as safety net) it reconciles
the desired state:

```
 ┌───────────────┐                                                  ┌───────────────┐
 │  OVN SB DB    │                                                  │  OVN NB DB    │
 │               │                                                  │               │
 │ Port_Binding  │─── read ───┐                          ┌── read ──│ NAT           │
 │ Chassis       │            │                          │  write ──│ Logical_Router│
 └───────────────┘            │                          │          │ Static_Route  │
                              │                          │          │ MAC_Binding   │
                              ▼                          ▼          └───────────────┘
                  ┌──────────────────────────────────────────────┐
                  │                 ovn-network-agent            │
                  │                                              │
                  │  ┌────────────┐    ┌──────────────────────┐  │
                  │  │  OVSDB IDL │    │   Event Processing   │  │
                  │  │  Monitors  │───►│                      │  │
                  │  └────────────┘    │  Fast: failover <10ms│  │
                  │                    │  Normal: debounce 500ms │
                  │                    └──────────┬───────────┘  │
                  │                               │              │
                  │                    ┌──────────▼───────────┐  │
                  │                    │     Reconciler       │  │
                  │                    │                      │  │
                  │                    │  1. Compute desired  │  │
                  │                    │     IPs (FIPs, SNATs,│  │
                  │                    │     LRP gws, PF VIPs)│  │
                  │                    │  2. Ensure OVS flows │  │
                  │                    │  3. Ensure OVN GW ──────────► OVN NB
                  │                    │     routing + active │  │    (default route,
                  │                    │     priority lead    │  │     MAC binding,
                  │                    │  4. Sync routes      │  │     priority lead)
                  │                    │  5. Verify + re-add  │  │
                  │                    └──┬──────┬──────┬─────┘  │
                  └───────────────────────┼──────┼──────┼────────┘
                                          │      │      │
                           ┌──────────────┘      │      └───────────────┐
                           ▼                     ▼                      ▼
                 ┌───────────────────┐ ┌───────────────────┐ ┌───────────────────┐
                 │ Kernel (netlink)  │ │ OVS (ovs-ofctl)   │ │ FRR (vtysh)       │
                 │                   │ │                   │ │                   │
                 │ /32 routes +rules │ │ MAC-tweak flows   │ │ ip route in VRF   │
                 │ proxy ARP on br-ex│ │ Hairpin flows     │ │ → BGP announce    │
                 │                   │ │ on br-ex          │ │                   │
                 └───────────────────┘ └───────────────────┘ └───────────────────┘
```

For each locally-active router the agent:

1. Writes a **default route** (`0.0.0.0/0 via <virtual-gw>`) and **static
   MAC binding** into OVN NB — the
   [virtual gateway](gatewayless-networks) makes reply traffic exit the
   logical router without a real upstream gateway. The binding carries the
   MAC of the router's own segment interface (`br-ex` for flat networks,
   `br-ex.<tag>` for VLAN networks).
2. Boosts the **active `Gateway_Chassis` priority** to `max(max peer + 1, 2)`
   so a previously-drained peer that returns at priority 1 cannot trigger
   a reverse failover (see [gateway drain](gateway-drain#priority-semantics)).
3. Installs **OVS MAC-tweak flows** on `br-ex`, one pair per localnet patch
   port — rewrites the destination MAC on packets arriving from OVN to the
   MAC of that segment's kernel interface so the kernel accepts them.
4. Installs **OVS hairpin flows** on `br-ex` — reflects same-chassis
   cross-router traffic back into OVN via `output:in_port` with rewritten
   MACs, each flow bound to the patch port of the segment the target FIP's
   external network is on.
5. Reconciles **per-network veth-leak routes and policy rules** so the
   provider subnet's reply traffic crosses from the default VRF into
   `vrf-provider` for BGP delivery.
6. Creates `/32` **kernel routes** (with `ip rule` entries when using a
   dedicated routing table) on the segment's kernel interface for every
   FIP, SNAT IP, router LRP gateway IP, and configured port-forward VIP.
7. Creates `/32` **FRR static routes** in `vrf-provider` for the same set
   so BGP announces them to the external fabric.
8. Triggers a **BGP outbound soft-refresh** only when routes are removed
   (withdrawals) — additions rely on FRR's normal route redistribution to
   avoid disrupting existing BGP announcements.
9. **Verifies** all desired routes (FRR and kernel) after every route change
   and re-adds any that went missing as a safety net.

Port forwarding (DNAT) reconciliation runs independently of router locality
— configured VIPs always get kernel/FRR routes so they remain announced via
BGP even on a chassis that currently hosts no OVN gateway.

## Data plane

This diagram shows the complete packet path on a gateway node. The upper
half (default VRF) handles OVN traffic and kernel routing. The lower half
(`vrf-provider`) handles BGP announcement and external delivery. The veth
pair managed by the agent (`--veth-leak-enabled`) bridges the two VRFs.

```
 ┌──────────────────────────────────────────────────────────────────────────────────────┐
 │                                    Gateway Node                                      │
 │                                                                                      │
 │  ┌───────────────────────────────── Default VRF ──────────────────────────────────┐  │
 │  │                                                                                │  │
 │  │   ┌───────────────────────────┐             ┌──────────────────────────────┐   │  │
 │  │   │  br-ex (provider bridge)  │ patch port  │  br-int (OVN integration)    │   │  │
 │  │   │                           │◄───────────►│                              │   │  │
 │  │   │  proxy ARP enabled        │             │  OVN Logical Router:         │   │  │
 │  │   │  bridge IP 169.254.169.254│             │   DNAT: FIP → VM IP          │   │  │
 │  │   │  /32 route per FIP        │             │   SNAT: VM IP → FIP          │   │  │
 │  │   │  MAC-tweak flows (0x999)  │             │   default route → .254 ¹     │   │  │
 │  │   │  Hairpin flows   (0x998)  │             │   MAC binding → br-ex MAC    │   │  │
 │  │   │                           │             │                              │   │  │
 │  │   └─────────┬─────────────────┘             │                              │   │  │
 │  │        physical NIC                         │  ¹ virtual gateway (last     │   │  │
 │  │        (uplink)                             │    usable IP in subnet)      │   │  │
 │  │             │                               └──────────────┬───────────────┘   │  │
 │  │             │                                              │                   │  │
 │  │             │                                     VM (10.0.0.5)                │  │
 │  │             │                                     FIP: 203.0.113.10            │  │
 │  │                                                                                │  │
 │  │   ┌─────────────────┐                                                          │  │
 │  │   │  veth-default   │    ip rule: from <provider-net> → lookup table 200       │  │
 │  │   │  169.254.0.1/30 │    table 200: default via 169.254.0.2                    │  │
 │  │   └────────┬────────┘                                                          │  │
 │  └────────────┼───────────────────────────────────────────────────────────────────┘  │
 │          veth pair                                                                   │
 │  ┌────────────┼────────────────────── vrf-provider ──────────────────────────────┐   │
 │  │            │                                                                  │   │
 │  │   ┌────────▼────────┐        ┌──────────────────────────┐                     │   │
 │  │   │  veth-provider  │        │        FRR / BGP         │                     │   │
 │  │   │  169.254.0.2/30 │        │   announces /32 routes   │                     │   │
 │  │   └─────────────────┘        │   via BGP peering        │                     │   │
 │  │                              └─────────────┬────────────┘                     │   │
 │  │                                            │                                  │   │
 │  │   <net>/24 via 169.254.0.1                 │  (→ default VRF, return path)    │   │
 │  │   <FIP>/32 via 169.254.0.1                 │  (agent-managed, per FIP)        │   │
 │  └────────────────────────────────────────────┼──────────────────────────────────┘   │
 └───────────────────────────────────────────────┼──────────────────────────────────────┘
                                                 │ BGP peering (/32 FIP routes)
                                                 ▼
                                         ┌───────────────┐
                                         │   External    │
                                         │  BGP Router / │
                                         │    Fabric     │
                                         └───────────────┘
```

### Forward path (external client → VM)

1. External router learns `198.51.100.10/32` via BGP from FRR in
   `vrf-provider`.
2. Packet (`dst=198.51.100.10`) arrives at `br-ex` via the physical NIC.
3. Kernel finds the `/32` route on `br-ex` (scope link); proxy ARP resolves
   the FIP to the bridge MAC.
4. OVS MAC-tweak flow rewrites the destination MAC to `br-ex` MAC and
   passes the packet to `br-int`.
5. OVN Logical Router applies DNAT: `198.51.100.10` → `10.0.0.5`.
6. Packet is delivered to the VM on its internal network.

### Return path (VM → external client)

1. VM sends reply (`src=10.0.0.5`, `dst=external client`).
2. OVN Logical Router applies SNAT: source becomes `198.51.100.10`.
3. OVN forwards via default route (`0.0.0.0/0 via .254` — the virtual
   gateway) + static MAC binding (`.254` → `br-ex` MAC) → packet exits
   through `br-ex`.
4. Packet leaves `br-ex` with `src=198.51.100.10` (falls in a provider
   network range).
5. Policy rule `from <net> → lookup table 200` matches the source address.
6. Table 200 routes via `169.254.0.2` → veth pair → packet enters
   `vrf-provider`.
7. FRR/BGP in `vrf-provider` delivers the packet to the external fabric.

### VRF route leaking

The agent creates `/32` FRR routes inside `vrf-provider`, but reply traffic
from OVN arrives in the default VRF on `br-ex`. A veth pair bridges the two
VRFs so that:

- **Default VRF → `vrf-provider`**: an `ip rule` matches the source address
  of reply packets against the discovered (or configured) provider networks
  and redirects them into routing table 200. Table 200 has a default route
  via `169.254.0.2` (the `veth-provider` end), which moves the packet into
  `vrf-provider` for BGP delivery.
- **`vrf-provider` → Default VRF**: network routes in `vrf-provider` (e.g.
  `192.0.2.0/24 via 169.254.0.1`) send return traffic back through the veth
  pair into the default VRF for normal kernel delivery.

The agent creates the veth pair and assigns link-local addresses at startup
(`--veth-leak-enabled`, on by default). Per-network policy rules and routes
are reconciled dynamically — networks are either auto-discovered from OVN
`Logical_Router_Port.Networks` or taken from the static `network_cidr`
configuration. On shutdown, all resources are cleaned up.

### VLAN provider networks

The diagram above shows a single flat provider network, where the kernel
side of the data path lives on the bridge device itself. When a node serves
**multiple provider networks** as VLAN localnet segments on the same
physnet, each segment gets its own kernel path: the agent creates an 802.1Q
subinterface `br-ex.<tag>` per VLAN segment, carrying that segment's bridge
IP, proxy ARP, and per-IP `/32` routes. Traffic that OVN emits tagged on
the provider bridge reaches the kernel through the matching subinterface,
and the per-patch-port MAC-tweak flows rewrite the destination MAC to that
subinterface's MAC. Flat segments keep the untagged path on `br-ex`
unchanged. See
[gatewayless networks](gatewayless-networks#multiple-provider-networks-vlan-segments)
for the segment discovery and lifecycle details.
