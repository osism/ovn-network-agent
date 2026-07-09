# Gateway drain mode

## Background: the problem

When the agent shuts down (e.g. for a rolling upgrade or node maintenance),
two things happen nearly simultaneously:

1. **BGP withdrawal** — FRR withdraws the `/32` routes for all FIPs on this
   node, so the external fabric stops sending traffic here within seconds.
2. **OVN BFD failover** — OVN detects that the gateway chassis is gone and
   migrates chassisredirect ports to standby chassis. This relies on BFD
   timeouts (typically 3×1s = 3 seconds) or periodic probing. See
   [BFD failover detection](./bfd-failover-detection) for measuring and
   lowering this floor.

The problem is the **gap between these two events**. During the window where
BGP has already withdrawn routes but OVN has not yet completed failover,
traffic that was already in flight (or cached by upstream routers) arrives
at the node and gets blackholed — OVN still considers this chassis active,
but the routes are gone. This causes a brief but measurable traffic
disruption on every shutdown.

## Solution: pre-shutdown priority drain

The agent solves this by **draining gateways before cleanup**. On
SIGINT/SIGTERM, before removing any routes or closing OVN connections, the
agent:

1. **Lowers its `Gateway_Chassis` priority to 0** in the OVN Northbound
   database for all locally-active router ports. Since standby chassis have
   priority >= 1, `ovn-northd` immediately begins migrating chassisredirect
   ports to standby chassis.
2. **Polls the SB `Port_Binding` table** until all chassisredirect ports
   have moved away from this chassis (or the drain timeout expires).
3. **Proceeds with normal cleanup** — by this point OVN has already
   migrated traffic to another chassis, so the BGP withdrawal and route
   cleanup cause zero disruption.

On the **next startup**, before the first reconciliation, the agent detects
drained entries (priority 0 on the local chassis) and **restores them to
priority 1** (standby level). This re-adds the chassis to the HA group as a
standby. The active chassis maintains a minimum priority of 2 via an
automatic **priority lead boost** during reconciliation (see [Priority
semantics](#priority-semantics)), which is strictly above the restore level
of 1 — preventing reverse failover without requiring a priority tie to
trigger the boost.

This inverts the shutdown order: OVN failover happens **first** (triggered
by the priority change), and BGP withdrawal happens **after** traffic has
already moved. The result is a hitless shutdown.

## Shutdown sequence

```
  SIGINT / SIGTERM received
          │
          ▼
  ┌───────────────────────────────────────────────────────┐
  │  1. DRAIN (if drain_on_shutdown=true)                 │
  │                                                       │
  │  For each Gateway_Chassis on this node (priority > 0):│
  │  ├─ Set priority to 0 in OVN NB                       │
  │  │  (batched in a single OVSDB transaction)           │
  │  │                                                    │
  │  ovn-northd recalculates chassisredirect bindings     │
  │  ├─ Standby chassis (priority >= 1) become active     │
  │  ├─ Traffic migrates to standby nodes                 │
  │  │                                                    │
  │  Poll SB Port_Binding until no chassisredirect        │
  │  ports remain on this chassis (or timeout expires)    │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
  ┌───────────────────────────────────────────────────────┐
  │  2. SETTLE (hold for drain_settle_delay)              │
  │                                                       │
  │  Keep advertising FIP /32 routes and OVS flows so     │
  │  this node still forwards external traffic            │
  │  (hairpinned to the new gateway chassis) while the    │
  │  takeover chassis finishes OVN programming, reconcile │
  │  and BGP advertisement. Skipped if nothing migrated.  │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
  ┌───────────────────────────────────────────────────────┐
  │  3. CLEANUP (if cleanup_on_shutdown=true)             │
  │                                                       │
  │  Remove kernel routes, FRR routes, OVS flows,         │
  │  bridge IP, nftables rules                            │
  │  (traffic already moved — no disruption)              │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
                    Agent exits
```

```
  Agent startup
          │
          ▼
  ┌───────────────────────────────────────────────────────┐
  │  RESTORE (if drain_on_shutdown=true)                  │
  │                                                       │
  │  For each Gateway_Chassis on this node with           │
  │  priority == 0:                                       │
  │  ├─ Set priority to 1 (standby level)                 │
  │  │  (batched in a single OVSDB transaction)           │
  │  │                                                    │
  │  Chassis rejoins HA group as standby                  │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
  ┌───────────────────────────────────────────────────────┐
  │  RECONCILE (includes priority lead boost)             │
  │                                                       │
  │  If this chassis is the active gateway:               │
  │  ├─ Compare local priority with peers in HA group     │
  │  ├─ If local priority <= max peer priority            │
  │  │  OR local priority < 2 (minimum active priority):  │
  │  │  boost to max(max peer + 1, 2)                     │
  │  │                                                    │
  │  This ensures the active chassis always has           │
  │  priority >= 2, strictly above the restore level (1), │
  │  preventing reverse failover even when all peers      │
  │  are drained.                                         │
  └───────────────────────┬───────────────────────────────┘
                          │
                          ▼
                 Normal reconciliation loop
```

## The takeover race and the settle delay

Draining the priority and polling until the chassisredirect port has
migrated away makes the **OVN-internal** failover hitless. But "the port
moved" is not the same as "the takeover chassis can forward external
traffic". The leaving node advertises each FIP as a BGP `/32`; so does the
takeover node, once it is ready. Between the two events there is a second
race:

1. The leaving node sees the port migrate and — without a hold — proceeds
   straight to cleanup, which **withdraws its FIP `/32` routes** from BGP.
2. The takeover node still has to react to the `Port_Binding` change,
   reconcile, install OVS flows and kernel routes, advertise its own FIP
   `/32`s in FRR, and wait for the upstream fabric to reconverge.

If step 1 wins, external traffic has no working path until step 2
finishes — observed as roughly 5 s of packet loss on a continuous ping
through a FIP.

Two mechanisms close this race:

- **Post-drain settle delay** (`drain_settle_delay`, default 3 s). After
  the poll loop confirms the ports have migrated, the leaving node holds
  before cleanup. During the hold it still advertises its FIP routes and
  keeps its OVS flows, so it continues forwarding external traffic —
  hairpinned to the new gateway chassis over the tunnel — while the
  takeover node comes up. The hold is bounded by `drain_timeout`, so total
  graceful shutdown never exceeds that budget, and it is skipped when there
  was nothing to drain.
- **BGP soft-refresh on the takeover node.** When the takeover node adds
  FIP `/32`s to FRR, the agent immediately issues `clear ip bgp … soft out`
  instead of waiting for FRR's normal redistribution timing. The soft
  refresh only re-evaluates outbound policy and re-sends routes — it never
  withdraws anything — so it shortens the takeover node's "ready" time
  (and also speeds up ungraceful failovers) at the cost of a small extra
  UPDATE burst.

A portion of the remaining gap is OVN-intrinsic (`ovn-northd` /
`ovn-controller` reprogramming) and cannot be closed from the agent; the
settle delay only has to cover the takeover node's reconcile and BGP
advertisement.

## Priority semantics

The agent lowers the priority to **0** rather than 1 because in typical
Neutron L3 HA setups, standby chassis already have priority 1. Lowering to
the same value would not trigger migration. Priority 0 is below any standby
chassis, guaranteeing that `ovn-northd` redistributes the chassisredirect
port.

On the next startup, drained entries (priority 0) are restored to **1**
(standby level), not to their original priority. This is intentional:
restoring the original priority would risk making this chassis the
highest-priority gateway again, triggering a reverse failover.

To prevent reverse failover, the agent implements an **active priority lead
boost**: during each reconciliation, the active gateway chassis ensures its
`Gateway_Chassis` priority is both strictly higher than all peers and at
least **2** (the minimum active priority). The minimum of 2 is critical
because without it, an active chassis at priority 1 with a drained peer at
priority 0 would see "already has the lead" and skip boosting — then when
the peer restores to 1, both are at the same priority and OVN's tiebreaker
can pick either one, causing an unintended switchback. The boost target is
`max(max peer priority + 1, 2)`. This ensures:

- After a failover, the new active chassis immediately establishes priority
  dominance (>= 2) even while the old chassis is still drained at 0.
- When the old chassis restarts and restores to priority 1, the active
  chassis is already at 2 — no tie, no switchback.
- The boost is idempotent: once the lead is established, subsequent
  reconciliations are no-ops.

## Configuration

For task-oriented setup (enabling, timeouts, when to disable), see
[Configure gateway drain](../guides/gateway-drain).
