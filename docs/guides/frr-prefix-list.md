# Configure the FRR prefix list

By default the agent maintains an FRR prefix-list named `ANNOUNCED-NETWORKS`
that controls which prefixes are eligible for BGP redistribution. On every
reconciliation cycle, the agent emits `permit <network> ge 32 le 32` entries
for each discovered (or manually configured) provider network so FRR will
re-advertise the `/32`s the agent exposes: the per-FIP static routes it writes,
and — filtered through the same list — the connected routes of any managed
port-forward VIP addresses.

## Defaults

The relevant setting is `frr_prefix_list` (CLI: `--frr-prefix-list`, env:
`OVN_NETWORK_FRR_PREFIX_LIST`). It defaults to `ANNOUNCED-NETWORKS`.

```yaml
# Override the prefix-list name
frr_prefix_list: "MY-ANNOUNCED-PREFIXES"

# Or disable prefix-list management entirely
frr_prefix_list: ""
```

When set to an empty string the agent does not touch the prefix list at all
— useful if you manage BGP filtering with an external tool.

## What the agent emits

Once provider networks are known (auto-discovered from
`Logical_Router_Port.Networks` or specified via `network_cidr`), the agent
keeps the prefix list synchronised with `permit <network> ge 32 le 32`
entries for each network. In addition, every announceable port-forward VIP
gets its own exact `permit <vip>/32` entry — the VIP announces through its
connected route filtered by this list, and its address need not lie inside
any network the gateway currently hosts, so a network entry cannot be relied
on to cover it. Entries the agent no longer manages (removed networks,
dormant VIPs) are removed during the next reconciliation.

Each cycle also verifies the entries against bgpd's own copy of the list
(`vtysh -d bgpd`), not just the merged view across all FRR daemons. The two
can diverge: a vtysh write issued while FRR was restarting reaches only the
daemons already accepting connections, and an entry that landed in zebra but
missed bgpd silently blocks the network's announcements while looking present
in `show running-config`. A line missing from bgpd's copy is re-applied with
its existing sequence number, which repairs bgpd and is a no-op for the
daemons that already have it. When bgpd itself is unreachable the check is
skipped for that cycle and retried on the next one.

The prefix list itself must already be referenced from your BGP configuration.
There are two reference points, and a complete gateway config uses both:

1. The neighbor outbound filter — `neighbor <peer> prefix-list ANNOUNCED-NETWORKS
   out` — which gates what is sent to the upstream peer.
2. The route-map on `redistribute connected` — `redistribute connected route-map
   ANNOUNCE-CONNECTED`, where `route-map ANNOUNCE-CONNECTED permit 10` carries
   `match ip address prefix-list ANNOUNCED-NETWORKS`. This is what lets a managed
   port-forward VIP's connected route be announced while keeping the underlay
   `/30`s out (see the
   [port-forwarding guide](port-forwarding#vip-address-management)).

The route-map reference matters on a standby: the agent empties the list there,
and an undefined list inside a route-map `match` is a no-match (nothing
exported), whereas an undefined list in the neighbor filter alone is treated as
permit-all.

The agent only manages the prefix-list contents — it does not modify your BGP
configuration.

## Where to go next

- [Configuration reference](../reference/configuration) — every flag, env
  var, and config key.
- [Architecture](../explanation/architecture) — where the prefix list fits in
  the control plane.
