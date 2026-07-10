# Configure gateway drain

The agent drains HA gateways on shutdown by default to eliminate the failover
gap between BGP withdrawal and OVN BFD detection. For the conceptual model
and the shutdown sequence diagrams, see
[Gateway drain mode](../explanation/gateway-drain).

## Default behavior

Drain mode is **enabled by default** with a 60-second timeout and a
500-millisecond post-readiness safety margin:

```yaml
# Enable/disable drain (default: true)
drain_on_shutdown: true

# Maximum time to wait for migration and the takeover handshake (default: 60s)
# After this timeout, the agent proceeds with shutdown even if migration
# or the readiness handshake has not completed.
drain_timeout: "60s"

# Safety margin held after the takeover chassis signals readiness (default: 500ms)
# The node first waits for the takeover chassis to signal it can forward
# (bounded by drain_timeout), then holds this additional margin before
# cleanup. Set to 0 to disable the wait and the margin. See "Settle
# delay" below.
drain_settle_delay: "500ms"
```

## Override via CLI flags

```bash
ovn-network-agent --drain-on-shutdown=false                 # disable drain
ovn-network-agent --drain-timeout 120s                      # increase timeout
ovn-network-agent --drain-settle-delay 1s                   # longer safety margin
```

## Override via environment variables

```bash
OVN_NETWORK_DRAIN_ON_SHUTDOWN=false                         # disable drain
OVN_NETWORK_DRAIN_ON_SHUTDOWN=true                          # enable drain (over a config file that disables it)
OVN_NETWORK_DRAIN_TIMEOUT=120s                              # increase timeout
OVN_NETWORK_DRAIN_SETTLE_DELAY=1s                           # longer safety margin
```

## Settle delay

Migration of the chassisredirect port away from this node does **not**
mean the takeover chassis is ready to receive external traffic. Before it
is ready, `ovn-northd` recompute, `ovn-controller` flow programming, the
takeover agent's reconcile and its FRR/BGP advertisement all still have to
happen. If the leaving node withdrew its FIP routes the instant the port
moved, external traffic would be blackholed for that gap (observed as
~5 s of packet loss).

The agents close that race with an active handshake instead of a fixed
sleep: once the ports have migrated, the leaving node waits for the
takeover chassis to stamp a readiness marker on the managed default route
(written only after the takeover node has actually announced its FIP
routes), and only then holds `drain_settle_delay` as a final safety
margin before cleanup. Throughout the wait and the margin it keeps
advertising its FIP `/32` routes and OVS flows, forwarding external
traffic (hairpinned to the new gateway chassis over the tunnel) until the
takeover chassis is provably up. For the full mechanism see
[The takeover handshake](../explanation/gateway-drain#the-takeover-handshake).

`drain_settle_delay` is therefore just the margin held *after* the
readiness signal, not the whole hold — which is why its default dropped
from `3s` to `500ms`. The entire handshake (readiness wait plus margin)
is bounded by `drain_timeout`, so total graceful shutdown never exceeds
that budget. Set `drain_settle_delay` to `0` to disable the readiness
wait and the margin entirely and have cleanup run as soon as the ports
migrate.

**Rolling upgrades.** A peer running a pre-handshake agent never writes
the readiness marker, so while you upgrade the agent fleet a drain waits
for the full `drain_timeout` before falling back (safe — it never
releases early, just slower). If you need faster drains during the
upgrade window, temporarily lower `drain_timeout` or set
`drain_settle_delay: 0`.

## When to disable drain

- **Single-chassis deployments** — if there is no standby chassis, lowering
  the priority has no effect and the timeout just delays shutdown.
- **Non-HA routers** — routers without multiple `Gateway_Chassis` entries
  cannot fail over; drain is a no-op (the agent detects this and skips
  immediately).
- **Environments where Neutron manages priorities** — if an external system
  actively manages `Gateway_Chassis` priorities and would conflict with the
  agent's changes.
