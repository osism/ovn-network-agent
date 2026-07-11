# Gatewayless provider networks

## Background: the problem

In a traditional OpenStack deployment, the provider network has a real
upstream gateway (e.g. a physical router at `.1`). OVN uses this gateway IP
as the nexthop for its default route, so SNAT reply traffic naturally exits
the logical router and reaches the physical network.

When the provider network is configured with **`disable_gateway_ip: true`**
(gatewayless mode), there is no physical upstream gateway at all — all
external traffic is routed purely via BGP `/32` announcements. This creates
a problem: OVN's logical router has no nexthop for its default route, so
reply traffic (after SNAT) has no way to leave the logical router.

## Solution: the virtual gateway ("magic gateway")

The agent solves this by inventing a **virtual gateway IP** that does not
correspond to any real device. It picks the **last usable host address** in
the provider subnet (broadcast address minus one):

| Subnet | Virtual gateway IP |
|--------|--------------------|
| `198.51.100.0/24` | `198.51.100.254` |
| `192.168.42.0/23` | `192.168.43.254` |
| `10.0.0.0/16` | `10.0.255.254` |
| `172.16.0.0/30` | `172.16.0.2` |

The computation uses the first IPv4 CIDR found on the logical router's
external port (`Logical_Router_Port.Networks`).

For each locally-active router, the agent writes two entries into the OVN
Northbound database:

1. **Default route** — `0.0.0.0/0 via <virtual-gw>` on the logical router,
   so OVN knows where to send reply traffic after SNAT.
2. **Static MAC binding** — maps the virtual gateway IP to the MAC of that
   router's segment kernel interface (the `br-ex` MAC for flat networks,
   the `br-ex.<tag>` MAC for VLAN networks — see
   [multiple provider networks](#multiple-provider-networks-vlan-segments)),
   so OVN can resolve the nexthop without sending ARP requests that nobody
   would answer.

Together, these two entries trick OVN into forwarding SNAT reply packets out
of the logical router's external port onto `br-ex`, where the kernel and FRR
take over for BGP delivery. The virtual gateway IP itself is never used as an
actual destination — it only serves as the logical nexthop that makes OVN's
routing pipeline work.

Both entries are tagged with `ExternalIDs["ovn-network-agent"] = "managed"`
so the agent can track and clean them up. Additionally, managed static
routes carry `ExternalIDs["ovn-network-agent-chassis"]` set to the owning
chassis hostname, enabling stale chassis cleanup by surviving agents when a
node dies without graceful shutdown. If a default route already exists that
was **not** created by the agent (i.e. a real gateway configured by
OpenStack), the agent leaves it untouched.

For the OpenStack-side configuration that triggers the gatewayless path
(Ansible / openstack CLI), see
[Create a gatewayless provider network](../guides/gatewayless-provider-network).

## Failover behavior

On HA failover (chassisredirect port moves to a different chassis), the agent
on the new active node **updates the static MAC binding** to point to its
own segment interface MAC. This ensures reply traffic is forwarded to the
correct physical node without requiring any change to the logical route
itself.

## MAC-tweak flows on br-ex

Packets leaving OVN via `br-int` arrive on a patch port of `br-ex` with a
destination MAC set by OVN's logical pipeline — not the MAC of any kernel
interface. The Linux kernel would drop these packets because the
destination MAC does not match a local interface. To fix this, the agent
installs OVS flows (cookie `0x999`, priority 900) on `br-ex` **per localnet
patch port** that **rewrite the destination MAC** to the MAC of that
segment's kernel interface for all packets arriving on the port:

```
cookie=0x999,priority=900,ip,in_port=<patch-port>,actions=mod_dl_dst:<segment-iface-mac>,NORMAL
```

This allows the kernel to accept and route the packets normally (via the
`/32` kernel routes and policy rules into `vrf-provider` for BGP delivery).
With a single flat network there is exactly one patch port and the segment
interface is `br-ex` itself, which is the pre-multi-network behavior.

## Hairpin OVS flows on br-ex

When two OVN logical routers are both active on the same chassis, a FIP on
router-A trying to reach a FIP on router-B creates an asymmetric failure:
OVN sends the packet out via the localnet port to `br-ex`, the MAC-tweak
flow delivers it to the kernel, but the kernel has no local address for the
destination FIP and either drops or loops the packet. The same traffic works
fine from a *different* chassis because it arrives via the physical network
and OVN processes it correctly.

The agent installs per-IP **hairpin flows** (cookie `0x998`, priority 910)
that intercept packets from OVN destined for a locally-managed FIP and
**reflect them back through the same patch port** using `output:in_port`.
OVN then processes the reflected packet as if it arrived from the external
network, applying the correct DNAT/ICMP handling on the destination router.
Each flow is bound to the localnet patch port of the segment the target
FIP's external network is on, so the reflection stays within that segment.

Both source and destination MACs are rewritten:

- **`dl_src`** is set to the segment interface's MAC so the reflected packet
  appears as external traffic to OVN, avoiding loop detection.
- **`dl_dst`** is set to the owning router port's MAC so OVN's L2 lookup on
  the external logical switch delivers the packet to the correct router
  (without this, the original destination MAC may be unresolved when OVN's
  ARP resolution between co-located routers has not completed).

```
cookie=0x998,priority=910,ip,in_port=<patch-port>,ip_dst=<fip>/32,actions=mod_dl_src:<segment-iface-mac>,mod_dl_dst:<router-port-mac>,output:in_port
```

Priority 910 ensures hairpin fires **before** the MAC-tweak flow (priority
900), so locally-managed IPs are reflected into OVN while all other traffic
still falls through to MAC-tweak and exits to the physical network normally.
The hairpin flows are reconciled alongside the MAC-tweak flows and removed
when no routers are locally active.

## Multiple provider networks (VLAN segments)

A gateway node can announce FIPs from **more than one provider network** at
the same time, using VLAN-tagged localnet networks on the same physnet
(e.g. an operator-owned provider network per customer pool, each shared to
selected projects via Neutron RBAC `access_as_external`). No configuration
is needed — multi-network support activates automatically from discovery,
and a deployment with a single flat network behaves exactly as before.

### Segment discovery

For every locally-active router, the agent resolves the **localnet
segment** of its external network from the OVN Southbound database: the
gateway port's peer patch `Port_Binding` shares a datapath with the
external switch's `localnet` port, whose `options:network_name` names the
physnet and whose `tag` carries the optional VLAN ID. On the OVS side, the
segment's localnet port name is matched to the provider-bridge patch port
via the `external_ids:ovn-localnet-port` key that ovn-controller stamps on
the `Port` row.

Neutron RBAC (`access_as_external`) is transparent to this mechanism: the
agent only reads OVN NB/SB and never the Neutron API, so a non-shared,
RBAC-shared provider network behaves identically to an admin-created
`--external` network.

### Per-segment data path

For each **VLAN segment**, the agent creates a kernel 802.1Q subinterface
`<bridge_dev>.<tag>` (e.g. `br-ex.101`) on the provider bridge and moves
the segment's kernel path onto it: the bridge IP, proxy ARP, and the
per-IP `/32` routes. VLAN-tagged traffic that OVN emits on the provider
bridge reaches the kernel through this subinterface — on the untagged
bridge device it would bypass the kernel routing path entirely. **Flat
segments** keep the pre-existing behavior on the bridge device itself.

Subinterfaces created by the agent are marked with the link alias
`ovn-network-agent` so that pruning (failover, network removal, restart)
and shutdown teardown only ever delete links the agent owns. A
pre-provisioned subinterface with the right name is adopted as-is and
never deleted. Because `<bridge_dev>.<tag>` must fit the kernel's
15-character interface-name limit, an over-long combination causes the
segment to be skipped with an error log (the default `br-ex` is always
safe).

MAC-tweak and hairpin flows are installed per localnet patch port as
described above, and each router's virtual-gateway MAC binding is written
with the MAC of its own segment interface. Note that a VLAN subinterface
inherits the bridge MAC, so the per-segment MACs are typically identical —
the agent still resolves them per segment. Failover of one router tears
down only that router's segment resources; other segments on the node are
untouched.

A router whose segment cannot be resolved (no localnet row, or no patch
port carrying the `ovn-localnet-port` key) falls back to the legacy
single-patch-port data path — the first patch port on the bridge with the
kernel path on the bridge device — which keeps flat single-network
deployments bit-compatible. With several patch ports on the bridge this
fallback is ambiguous; the agent logs an error and proceeds, which is no
worse than the pre-multi-network nondeterminism but now visible. Note that
a deployment that already had two patch ports on the bridge (where the
agent nondeterministically served only one network) starts serving both
after an upgrade — that is the intended fix.

### Scope

- **Cross-segment hairpin** (FIP on VLAN 101 → FIP on VLAN 102 on the same
  chassis) is out of scope: hairpin flows are installed per segment only.
  Cross-segment same-chassis traffic falls through MAC-tweak into the
  kernel, which can route between the segment interfaces via the per-IP
  `/32` routes, but this path is not asserted by tests.
- The **IPv6 kernel path** is out of scope (status quo): bridge IP, proxy
  ARP, `/32` routes, and the virtual gateway are IPv4-only; the per-port
  IPv6 MAC-tweak flow variant is kept. Non-IPv4 NAT and LRP addresses are
  filtered out of the kernel/FRR desired set at ingest, so a dual-stack
  router announces its v4 FIPs normally while its v6 OVS flow variants
  remain. Full IPv6 support is tracked in
  [#85](https://github.com/osism/ovn-network-agent/issues/85) /
  [#70](https://github.com/osism/ovn-network-agent/issues/70).

The `localnet_segments` gauge reports how many segments are currently
bound — see the [metrics reference](../reference/metrics).
