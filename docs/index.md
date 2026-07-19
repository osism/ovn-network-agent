---
layout: home
hero:
  name: ovn-network-agent
  text: Event-driven networking for OVN
  tagline: A real-time daemon that watches OVN databases via OVSDB and synchronizes Floating IP routes, BGP announcements, and DNAT port forwards on gateway nodes.
  actions:
    - theme: brand
      text: First agent on a test host
      link: /tutorials/first-agent
    - theme: alt
      text: Configuration reference
      link: /reference/configuration
    - theme: alt
      text: View on GitHub
      link: https://github.com/osism/ovn-network-agent
features:
  - title: Event-driven reconcile
    details: Connects to OVN Southbound and Northbound via OVSDB IDL, reacts to Port_Binding, Chassis, NAT, and Logical_Router changes in real time, with a periodic safety-net reconcile.
  - title: Gatewayless provider networks
    details: Invents a virtual "magic gateway" and writes default routes plus static MAC bindings into OVN NB so SNAT reply traffic exits the logical router without a physical upstream gateway.
  - title: BGP /32 announcement
    details: Installs kernel /32 routes, IP rules, and FRR static routes in a dedicated VRF so FRR announces each FIP and SNAT IP to the external fabric.
  - title: Port forwarding (DNAT)
    details: Forwards anycast VIPs to internal backends with sticky multi-backend hashing, conntrack-based return routing, and source-selective masquerade for FIP and router-SNAT hairpin.
  - title: Hitless gateway drain
    details: Lowers Gateway_Chassis priority to zero on SIGINT/SIGTERM so OVN migrates traffic before BGP withdrawal, eliminating the failover gap on rolling upgrades.
  - title: Prometheus metrics
    details: Exposes reconcile counters, drain durations, OVN connection state, and stale-chassis cleanup events on an optional HTTP endpoint, with suggested alert rules.
---

## Known limitations

Weigh these current constraints before planning a deployment:

- **IPv4-only FIP routing plane** — kernel routes, FRR announcements, and the
  virtual-gateway path are IPv4 only. IPv6 NAT and LRP addresses are filtered
  out of the kernel/FRR desired set at ingest, while the per-port IPv6
  MAC-tweak OVS flow variant is kept. See the scope note in
  [gatewayless provider networks](./explanation/gatewayless-networks) and issues
  [#85](https://github.com/osism/ovn-network-agent/issues/85) /
  [#70](https://github.com/osism/ovn-network-agent/issues/70).
- **No runtime configuration reload** — configuration is read once at startup
  and there is no SIGHUP reload, so restart the agent to apply any change,
  including the TLS certificate files for `ssl:` OVN remotes
  ([#91](https://github.com/osism/ovn-network-agent/issues/91)).
- **Port forwarding is IPv4-only with modulo hashing** — multi-backend VIPs
  distribute clients with `jhash ip saddr mod N` over the backend count. This
  is sticky per client but not a consistent hash, so adding or removing a
  backend remaps roughly `(N-1)/N` of clients, and there are no backend health
  checks. See
  [sticky load balancing](./guides/port-forwarding#sticky-load-balancing-multi-backend).
- **Single provider bridge per agent** — each agent instance drives one
  provider bridge (`bridge_dev`, default `br-ex`); VLAN localnet segments are
  all served on that one bridge as `<bridge_dev>.<tag>` subinterfaces.
- **Sizing and scale** — the OVSDB monitors replicate whole tables into every
  agent — Southbound `Port_Binding` and `Chassis`, and Northbound `NAT`,
  `Logical_Router`, `Logical_Router_Port`, `Logical_Router_Static_Route`,
  `Static_MAC_Binding`, and `Gateway_Chassis` — so an agent's memory footprint
  and reconcile time scale with the size of the whole cloud, not with
  node-local state alone.
