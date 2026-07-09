# BFD failover detection

## Background: where the failover time actually goes

A *graceful* shutdown is handled by [gateway drain mode](./gateway-drain): the
agent migrates the chassisredirect ports away before it stops announcing
routes, so there is no blackhole window.

An *ungraceful* failure — a node crash, a network partition, an
`ovn-controller` crash — has no such handshake. Nobody tells OVN that the
gateway chassis is gone; OVN has to *notice*. It notices via BFD, the
sub-second liveness protocol running over the Geneve tunnels between chassis.
Until BFD declares the tunnel down, OVN still believes the dead chassis owns
the `chassisredirect` Port_Binding and will not move it anywhere.

That detection time dominates the total failover time:

| Phase | Typical duration |
|-------|------------------|
| BFD declares the dead chassis' tunnel down | ~3 s with untuned timers |
| OVN moves the `chassisredirect` Port_Binding | tens of milliseconds |
| The surviving agent reacts and programs routes | < 1 s |

The agent's own reaction is already fast: an OVN event bypasses the reconcile
debounce entirely (`immediateStateRefresh`). Shaving time off the agent buys
nothing while the first row costs three seconds. **BFD detection is the
floor**, and until you measure it you do not know where that floor is.

## The two BFD layers

Two independent BFD layers bound an ungraceful failover, and they fail in
different ways:

**OVN tunnel BFD** runs between chassis over the Geneve tunnels.
`ovn-controller` enables it (`bfd:enable=true` on the tunnel interface) but
sets no timers, so OVS uses its own defaults. It decides *when OVN moves the
gateway*.

**FRR BGP BFD** runs between this node and the external fabric. It decides
*when the fabric stops sending traffic to a crashed node*. Without it, the
fabric keeps the dead node's `/32` announcements until the BGP hold timer
expires — typically far longer than OVN's failover. A node that crashes
outright cannot withdraw its own routes, so this gap is real precisely in the
case that matters most.

Tuning one and not the other leaves the slower one as the floor.

## Estimating the detection time

BFD declares a peer dead after `detect multiplier` intervals of silence, where
the interval is the slower of the two directions. The agent estimates each
layer from the state it can read locally:

- **OVN tunnels** (`ovs-vsctl find Interface type=geneve`, the `bfd` column):
  `mult × max(min_rx, min_tx)`. The OVS defaults are `min_rx=1000 ms`,
  `min_tx=100 ms`, `mult=3` — hence the ~3 s figure quoted above.
- **FRR sessions** (`vtysh -c "show bfd peers json"`):
  `detect-multiplier × max(receive-interval, remote-transmit-interval)`.

The worst case per layer is exported as
`ovn_network_agent_bfd_detect_seconds{layer="ovn"|"frr"}` and logged as a
warning when it exceeds `bfd_check_max_detect` (default `1s`). This check is
on by default and modifies nothing.

The gauge distinguishes three states that a plain number cannot:

| Value | Meaning |
|-------|---------|
| a duration | the estimated detection time on that layer |
| `+Inf` | nothing bounds the detection: the layer runs no BFD session, or a BGP neighbor in the VRF runs none |
| `NaN` | the layer could not be read — `ovs-vsctl` or `vtysh` failed |

`+Inf` is not a corner case: it is the state of every node before
`frr_bfd_manage` is enabled, and the alert rule `bfd_detect_seconds > 1` has to
catch it. A *single* BGP neighbor without a session is enough to raise it: the
fabric withdraws this node's `/32`s over every session it has, so the neighbor
that has none bounds the failover, however fast the others are. `NaN` matches no
comparison at all, which is the point — a layer the
agent cannot see says nothing about the failover time. A cycle that hits it
increments `ovn_network_agent_bfd_check_errors_total{layer}` and replaces the
last good value rather than leaving it in place, so a stale reading never
outlives the state it was taken from.

Two caveats on the OVN estimate. It reads only the **local** side's
configuration, and the negotiated interval is the maximum across *both*
endpoints — so the real detection time is at least the estimate, and lowering
timers on one node alone changes nothing. Timers have to be lowered
fleet-wide. And a tunnel without `bfd:enable=true` runs no session at all, so
it contributes nothing to the estimate.

## Lowering the floor

With `min_rx = min_tx = 150 ms` and the default multiplier of 3, the OVN floor
drops from ~3 s to ~450 ms. `ovn_bfd_manage` sets those keys on the local
Geneve tunnel interfaces; `frr_bfd_manage` does the equivalent for the VRF's
BGP sessions. Both default to off, toggle independently, and are documented in
the [configuration reference](../reference/configuration).

Aggressive timers are not free. Each session sends a packet every `min_tx`
milliseconds and every missed window is one step towards declaring the peer
dead, so lowering the timers raises the packet rate and makes the session
correspondingly less tolerant of scheduling jitter and transient loss. A
control plane under memory pressure or a busy `ovn-controller` can miss three
150 ms windows without anything actually being wrong; the result is a spurious
failover, which is more disruptive than the 3 s it saved. 150 ms is a
reasonable starting point on a healthy fabric — validate it under load before
rolling it out, and back off if you see flapping.

### `ovn_bfd_manage` and ovn-controller

The agent sets only `bfd:min_rx` and `bfd:min_tx`. It never touches
`bfd:enable`, which `ovn-controller` owns.

Every drifted tunnel is written by a single `ovs-vsctl` transaction, not one
per tunnel. A tunnel `ovn-controller` destroyed since the enumeration is skipped
by `--if-exists` rather than failing the transaction.

**Verify against your deployed OVN version that `ovn-controller` preserves
operator-set timer keys on the tunnel interfaces.** Some versions rewrite the
`bfd` column when they reprogram a tunnel. If yours does, the agent re-applies
the timers on the next reconcile — self-healing, but with a window in which the
timers are back to their defaults, and a log line on every reconcile cycle.

You do not have to take the agent's word for it. The estimate is always read
back from the `bfd` column, never taken from the values the agent just wrote,
so an `ovn-controller` that reverts them keeps `bfd_detect_seconds{layer="ovn"}`
pinned at the untuned floor. A metric that stays at `3` with `ovn_bfd_manage`
enabled means the writes are not sticking.

### `frr_bfd_manage` and the fabric

The agent discovers the VRF's **already-configured** BGP neighbors and adds
the `bfd` knob to them, attaching each to a `bfd profile` named
`ovn-network-agent` that carries the configured timers. It never configures
BGP peering itself: the AS number for `router bgp <as> vrf <vrf>` is read from
the running configuration, and a VRF with no `router bgp` stanza is an error
rather than an instance the agent creates.

Because the timers live in the profile, `bgpd` keeps reporting its own session
defaults (`3` / `300 ms` / `300 ms`) per neighbor — those are not the effective
values, and the agent does not compare against them. What it compares against is
the running configuration: a neighbor is left alone only when
`neighbor <addr> bfd profile ovn-network-agent` is already in the VRF's
`router bgp` stanza. A neighbor an operator gave a plain `neighbor <addr> bfd`
runs at `bgpd`'s defaults, so the agent attaches it to the profile anyway.

Each neighbor is attached at most once per agent start. Not every peering FRR
reports can be rendered back as `neighbor <addr> bfd profile ovn-network-agent`
— a peer-group member whose BFD FRR writes under the group is one — and
attaching such a neighbor on every reconcile would reinstall its BGP session
every 60 seconds. The agent logs the neighbors it has attached that never
appeared in the running configuration and leaves them alone; the `frr` layer
reads `+Inf` for as long as one of them has no BFD session.

The profile itself is written on the first reconcile after the agent starts, so
**a changed timer setting takes effect on agent restart**, not on the next
reconcile. It is written again whenever the running configuration no longer
carries it: FRR holds the profile only in its running state, and a
`systemctl reload frr` reapplies an `frr.conf` that has the neighbors' `bfd
profile` lines but not the profile stanza the agent never persisted.

Two operational prerequisites:

- `bfdd` must be running in FRR. Without it, `vtysh` reports no BFD peers and
  the `frr` layer reads `+Inf` — nothing bounds the fabric's route withdrawal.
- **The fabric side must enable BFD too.** BFD is a two-party protocol; a
  session where only one end is configured never comes up, and the `/32`
  withdrawal stays as slow as the BGP hold timer.

## What this does not change

BFD tuning lowers the *detection* floor. It does not change OVN's HA model
(`Gateway_Chassis` vs `HA_Chassis_Group`), and it does not help a graceful
shutdown — that path is already covered by
[gateway drain mode](./gateway-drain), which avoids detection entirely by
handing the gateway over before leaving.

## Configuration

For the option names, defaults, and env vars, see the
[configuration reference](../reference/configuration). For the exported metric
see the [metrics reference](../reference/metrics).
