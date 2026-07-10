# Configure gateway drain

The agent drains HA gateways on shutdown by default to eliminate the failover
gap between BGP withdrawal and OVN BFD detection. For the conceptual model
and the shutdown sequence diagrams, see
[Gateway drain mode](../explanation/gateway-drain).

## Default behavior

Drain mode is **enabled by default** with a 60-second timeout and a
3-second post-drain settle delay:

```yaml
# Enable/disable drain (default: true)
drain_on_shutdown: true

# Maximum time to wait for migration (default: 60s)
# After this timeout, the agent proceeds with shutdown even if some
# gateways have not yet migrated.
drain_timeout: "60s"

# Hold time after migration completes, before cleanup (default: 3s)
# Keeps this node advertising its FIP routes while the takeover chassis
# finishes coming up. Set to 0 to disable. See "Settle delay" below.
drain_settle_delay: "3s"
```

## Override via CLI flags

```bash
ovn-network-agent --drain-on-shutdown=false                 # disable drain
ovn-network-agent --drain-timeout 120s                      # increase timeout
ovn-network-agent --drain-settle-delay 5s                   # longer settle hold
```

## Override via environment variables

```bash
OVN_NETWORK_DRAIN_ON_SHUTDOWN=false                         # disable drain
OVN_NETWORK_DRAIN_ON_SHUTDOWN=true                          # enable drain (over a config file that disables it)
OVN_NETWORK_DRAIN_TIMEOUT=120s                              # increase timeout
OVN_NETWORK_DRAIN_SETTLE_DELAY=5s                           # longer settle hold
```

## Settle delay

Migration of the chassisredirect port away from this node does **not**
mean the takeover chassis is ready to receive external traffic. Before it
is ready, `ovn-northd` recompute, `ovn-controller` flow programming, the
takeover agent's reconcile and its FRR/BGP advertisement all still have to
happen. If the leaving node withdrew its FIP routes the instant the port
moved, external traffic would be blackholed for that gap (observed as
~5 s of packet loss).

`drain_settle_delay` closes that race: after the drain confirms the ports
have migrated, the agent keeps advertising its FIP `/32` routes and keeps
its OVS flows for this long, continuing to forward external traffic
(hairpinned to the new gateway chassis over the tunnel) until the takeover
chassis has come up. Only then does cleanup withdraw the routes.

The trade-off is shutdown time: a graceful drain takes up to
`drain_settle_delay` longer. The hold is bounded by `drain_timeout`, so
total graceful shutdown never exceeds that budget. Set `drain_settle_delay`
to `0` to disable the hold and have cleanup run as soon as the ports
migrate.

## When to disable drain

- **Single-chassis deployments** — if there is no standby chassis, lowering
  the priority has no effect and the timeout just delays shutdown.
- **Non-HA routers** — routers without multiple `Gateway_Chassis` entries
  cannot fail over; drain is a no-op (the agent detects this and skips
  immediately).
- **Environments where Neutron manages priorities** — if an external system
  actively manages `Gateway_Chassis` priorities and would conflict with the
  agent's changes.
