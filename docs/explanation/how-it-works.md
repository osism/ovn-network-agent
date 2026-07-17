# How the reconcile loop works

The agent monitors the OVN Southbound and Northbound databases and performs
targeted writes to both OVN NB (default routes, static MAC bindings) and the
local system (kernel routes, IP rules, FRR static routes, OVS flows on the
provider bridge).

1. **Connects** to OVN Southbound and Northbound databases via OVSDB IDL.
2. **Watches** for changes in real-time:
   - `Port_Binding` table (SB) — detects gateway chassis failover
     (chassisredirect changes bypass debouncing for fast reaction) and
     extracts SNAT IPs from `NatAddresses` on gateway patch ports for
     immediate route announcement before NB NAT entries exist.
   - `Chassis` table (SB) — detects chassis membership changes.
   - `NAT` table (NB) — detects Floating IP and SNAT assignments.
   - `Logical_Router` / `Logical_Router_Port` tables (NB) — maps NAT entries
     to their owning routers and auto-discovers provider network CIDRs from
     `Logical_Router_Port.Networks`.
3. **Sets up** the provider bridge at startup:
   - Adds a **link-local IP** to the bridge device so the kernel can perform
     ARP resolution.
   - Enables **proxy ARP** on the bridge device so the kernel responds to ARP
     requests for FIP addresses.
4. **Reacts** instantly to changes — for each router whose gateway is active
   on this chassis:
   - Installs **OVS MAC-tweak flows** on the provider bridge so the kernel
     accepts packets from OVN (rewrites destination MAC to `br-ex` MAC).
   - Installs **OVS hairpin flows** (per FIP, priority 910) that reflect
     same-chassis cross-router traffic back into OVN via `output:in_port`,
     rewriting both source MAC (`br-ex` MAC) and destination MAC (owning
     router port MAC) — see
     [Hairpin OVS flows](gatewayless-networks#hairpin-ovs-flows-on-br-ex).
   - Ensures a **default route** (`0.0.0.0/0 via <virtual-gw>`) and **static
     MAC binding** in OVN NB so reply traffic exits the logical router
     correctly (see [Gatewayless provider networks](gatewayless-networks)).
   - If configured, reconciles the **FRR prefix-list** with
     `permit <network> ge 32 le 32` entries for each discovered provider
     network. This is the BGP outbound filter, and a standby chassis empties
     it, so it is repopulated *before* the announce below — otherwise the
     takeover would advertise nothing.
   - Ensures `/32` **kernel routes** (with IP rules when using a dedicated
     routing table) and **FRR static routes** in the VRF for each FIP, SNAT
     IP, router LRP gateway IP, and (independent of router locality)
     configured port-forward VIP. Only **IPv4** addresses enter this
     route/announce plane; non-IPv4 NAT and LRP addresses are excluded until
     full IPv6 support ([#85](https://github.com/osism/ovn-network-agent/issues/85),
     [#70](https://github.com/osism/ovn-network-agent/issues/70)) lands. A
     single malformed OVN row (an invalid `external_ip`) degrades only its own
     FIP — it never fails the whole batch. The stability steps below
     deliberately run after this kernel/FRR route installation and BGP
     announce so a failover takeover announce is never gated behind them
     (issue #131).
   - **Route ownership** is explicit: every agent-created kernel route is
     tagged with the agent's route protocol (`proto 44`) and every FRR static
     route uses the agent's veth next-hop. Reconciliation — including standby
     cleanup — touches only those agent-owned routes, so operator-created
     kernel routes on the bridge and operator static routes in the VRF are
     never removed. On upgrade from a version that predates the protocol tag,
     still-desired routes are re-tagged automatically on the next reconcile;
     any stale pre-upgrade leftovers are indistinguishable from operator
     routes and must be cleaned up once by hand.
   - Applies the **active priority lead boost** to its `Gateway_Chassis`
     entries: when this chassis is active, its priority is raised to
     `max(max peer + 1, 2)` so a peer returning from drain (restored to 1)
     cannot trigger reverse failover — see
     [Priority semantics](gateway-drain#priority-semantics).
   - Reconciles **per-network veth-leak routes and policy rules** in the
     veth leak table (default 200) so SNAT reply traffic on the provider
     subnet crosses into `vrf-provider` for BGP delivery.
   - If no routers are locally active: removes all managed routes (port
     forward VIPs still get kernel/FRR routes if any are configured).
5. **Port forwarding (DNAT)** — optionally forwards traffic from anycast VIP
   addresses (on a loopback interface in the VRF) to internal backends.
   Supports multiple backends per rule with sticky source-IP hashing
   (`jhash`) for consistent client-to-backend mapping. Client IPs are
   preserved using connmark-based return routing through the veth pair.
   Backends on the same host are handled via dedicated OUTPUT chains
   (`output_ctzone`, `output_fwmark`) since locally-delivered traffic
   bypasses the FORWARD/POSTROUTING path. Per-rule `masquerade` control
   allows mixing local and remote backends on the same VIP. Two
   source-selective hairpin fixes are available: `hairpin_masquerade: true`
   covers FIPs on the same node (matches the provider CIDR), and
   `router_masquerade: true` covers instances behind a router without a FIP
   (matches the specific router SNAT IPs discovered from OVN). Both leave
   unrelated external clients unaffected and can be combined on the same VIP.
   A `forward_veth_guard` nftables chain restricts the veth return path to
   legitimate traffic only. Requires `nft` binary and `veth_leak_enabled:
   true`.
6. **Reconciles** periodically as a safety net (default: every 60s).
7. **Detects stale chassis** — when a node dies without graceful shutdown,
   surviving agents detect its chassis disappearing from the SB Chassis
   table and clean up its managed OVN NB entries (static routes and MAC
   bindings) after a configurable grace period (default: 5m, configurable
   via `stale_chassis_grace_period`, set to `0` to disable). Random jitter
   (0-30s) prevents multiple agents from cleaning up simultaneously.
8. **Drains gateways** on shutdown (SIGINT/SIGTERM) — before cleanup, the
   agent lowers its `Gateway_Chassis` priority to 0 in OVN NB, causing
   `ovn-northd` to migrate chassisredirect ports to standby chassis (priority
   >= 1). On the next startup, drained entries are restored to priority 1
   (standby level). The active chassis automatically maintains a minimum
   priority of 2 (above all possible standby peers) during reconciliation,
   preventing reverse failover even when a peer restores to priority 1. This
   eliminates the traffic disruption window between BGP route withdrawal and
   OVN BFD failover detection (see [Gateway drain mode](gateway-drain)).
   Enabled by default (`drain_on_shutdown: true`, `drain_timeout: 60s`).
9. **Cleans up** after drain — removes all managed routes, OVS flows, and
   the bridge IP before exiting (configurable via `cleanup_on_shutdown`).
