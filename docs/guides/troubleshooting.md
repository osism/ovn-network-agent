# Troubleshooting

This runbook is for the on-call operator diagnosing a live agent: the process
is running but Floating IPs are unreachable, an alert has fired, or a drain
misbehaved during a reboot. Every Prometheus alert shipped in
[`contrib/prometheus-rules.yaml`](https://github.com/osism/ovn-network-agent/blob/main/contrib/prometheus-rules.yaml)
links to its section on this page, so you can jump straight from the alert to
its causes, diagnosis, and remediation.

The commands below use the shipped default names. Substitute the values you
configured wherever they differ (see the [configuration
reference](../reference/configuration)).

## Is the process alive? Is the agent functional?

Run these two first — they separate a crashed unit from a running-but-stuck
agent:

```bash
systemctl status ovn-network-agent
journalctl -u ovn-network-agent -n 200 --no-pager
```

Liveness and readiness are two different questions, answered by two endpoints
on the metrics listener. Both require `metrics_listen` to be set (it is **off
by default**); enable it as shown in the [metrics guide](metrics).

- `/healthz` is **unconditional liveness**: it returns `200 ok` whenever the
  process is up, regardless of whether the agent can do useful work.
- `/readyz` reports whether the agent is **functional**. It returns `200`
  only when OVN NB and SB are connected (unless the agent runs
  port-forward-only), at least one reconcile has completed, and the last
  reconcile's route sync succeeded. Otherwise it returns `503` with one
  `unready: …` line per failing check.

```bash
curl -s http://127.0.0.1:9273/healthz
curl -s http://127.0.0.1:9273/readyz
```

If `/healthz` fails or the unit is not active, the process is down — restart it
and read the journal for the fatal error (see [Actionable error-level log
messages](#actionable-error-level-log-messages)). If `/healthz` passes but
`/readyz` returns `503`, the agent is alive but not doing its job; the
`unready:` lines tell you where to look next.

## Agent up but no routes appear

The instances behind a Floating IP are up, the agent is running, but the FIP is
unreachable from outside. Walk this checklist in order — it follows a packet
from the agent's intent down to the OVS flows on the wire.

All names below — `vrf-provider`, `ANNOUNCED-NETWORKS`, `br-ex`, tables `200`
and `201`, and the metrics address `127.0.0.1:9273` — are the shipped defaults.
Substitute the values you configured for `vrf_name`, `frr_prefix_list`,
`bridge_dev`, `veth_leak_table_id`, `port_forward_table_id`, and
`metrics_listen` respectively.

1. **Read `/readyz`.** Start with the agent's own verdict:

   ```bash
   curl -s http://127.0.0.1:9273/readyz
   ```

   `unready: ovn nb disconnected` / `unready: ovn sb disconnected` — the agent
   cannot see OVN; fix connectivity first. `unready: awaiting first reconcile`
   — the agent has connected but not completed a cycle yet. `unready: last
   reconcile failed` — a cycle ran but its route sync did not succeed; the
   journal has the reason.

2. **Read the `reconciling` journal line.** Each cycle logs one at info level:

   ```bash
   journalctl -u ovn-network-agent | grep reconciling | tail -1
   ```

   Interpret its fields:

   - `has_local_routers=false` (and `local_routers=0`) — **no logical router
     has its chassisredirect port active on this chassis.** This node is not
     the gateway for anything right now, so it announces nothing. On a standby
     this is normal; on the node you expect to be active, it means the
     chassisredirect port lives elsewhere (continue at step 3).
   - `desired_ips=0` — the agent found no IPs to route for (no FIPs, SNAT IPs,
     or port-forward VIPs). If you expect some, a `network_cidr` filter may be
     excluding them, or OVN has not published them yet.
   - `effective_networks=0` — no provider networks are in effect (none
     configured via `network_cidr` and none auto-discovered from OVN). The
     prefix-list and veth-leak reconciliation then have nothing to populate.

3. **Confirm the chassisredirect port is on this chassis.** A FIP is only
   announced from the node currently hosting the router's gateway:

   ```bash
   ovn-sbctl find Port_Binding type=chassisredirect
   ```

   Compare each row's `chassis` column against this node's chassis. If it
   points at another chassis, that node is the active gateway — this is
   expected on a standby, and the FIP should be announced from there.

4. **Confirm the FRR prefix-list is populated.** It is the BGP outbound
   filter; an empty list means nothing is advertised:

   ```bash
   vtysh -c 'show ip prefix-list ANNOUNCED-NETWORKS'
   ```

5. **Confirm the FRR static routes are present *and* selected.** A route can
   exist as a static route yet not be installed (unresolvable next-hop), in
   which case it is never advertised:

   ```bash
   vtysh -c 'show ip route vrf vrf-provider static'
   ```

   Routes marked as not selected are the same condition the
   [`OVNNetworkAgentInactiveRoutes`](#alert-ovnnetworkagentinactiveroutes) alert
   catches.

6. **Check the kernel routes.** The FIP `/32` and the policy-routing tables:

   ```bash
   ip route show <fip>/32
   ip route show table 200
   ip route show table 201
   ip rule show
   ```

   Table `200` holds the veth-leak default route (owned by
   `veth_leak_table_id`); table `201` holds the DNAT return route (owned by
   `port_forward_table_id`). The FIP `/32` lives in the main table unless
   `route_table_id` is set to a non-zero table, in which case query that table
   instead. In the rule listing, `iif veth-default lookup main` must sit one
   priority below the `from <network> lookup 200` rules (1999 and 2000 by
   default). If it is missing, traffic between FIPs whose routers live on
   different gateways is lost while external clients keep working, and the
   packet counters of `veth-default` (`ip -s link show veth-default`) climb
   far above the offered load: each packet loops through the veth pair until
   its TTL expires.

7. **Check the nftables table.** Note the family is `ip`, not `inet`:

   ```bash
   nft list table ip ovn-network-agent
   ```

8. **Check the OVS flows on the provider bridge.** Two cookie families matter —
   the MAC-tweak flows and the hairpin flows:

   ```bash
   ovs-ofctl dump-flows br-ex 'cookie=0x999/-1'
   ovs-ofctl dump-flows br-ex 'cookie=0x998/-1'
   ```

   Cookie `0x999` marks the per-segment MAC-tweak flows; `0x998` marks the
   same-chassis hairpin flows.

## Alert: OVNNetworkAgentRouteInstability

Fires on `consecutive_readds >= 3`.

**Meaning.** The agent's post-change verification found managed routes missing
and re-added them for at least three consecutive reconcile cycles. Something
outside the agent keeps deleting the FRR or kernel routes it installs.

**Likely causes.** A competing agent or script managing the same VRF/table, an
FRR daemon restarting or racing on `vtysh`, or a kernel route being flushed by
another controller. One further cause looks identical from this alert but is not
a competing writer at all: an unresolvable veth next-hop, which keeps the agent's
own routes out of the RIB so verification reports them missing every cycle. See
[`OVNNetworkAgentNexthopRepaired`](#alert-ovnnetworkagentnexthoprepaired).

**Diagnosis.** Look for the escalated error line `persistent route instability
detected: routes required re-adding for multiple consecutive cycles` (it
carries `consecutive_cycles` and `re_added_this_cycle`). If it is accompanied by
`veth next-hop is unresolvable`, the cause is the next-hop, not another writer —
follow that alert instead. Otherwise compare the FRR and kernel state against the
agent's intent using steps 5 and 6 of [Agent up but no routes
appear](#agent-up-but-no-routes-appear).

**Remediation.** Find and stop the other writer of these routes. The agent
re-adds routes each cycle, so connectivity is usually preserved, but the churn
must be resolved at its source — the agent cannot win a fight with a competing
controller.

## Alert: OVNNetworkAgentNexthopRepaired

Fires on `rate(nexthop_repairs_total[1h]) > 0`.

**Meaning.** zebra was missing the connected route for the veth `/30` that every
FIP static resolves through, so none of those statics entered the RIB, nothing
was redistributed, and the node advertised no FIPs at all — an outage on the
gateway that owns the traffic, even though the agent, FRR's `running-config` and
the kernel all looked correct. The agent detected this and re-notified the kernel
about the `veth-provider` address so zebra could relearn the route. The alert
reports that the repair ran; it does not mean the repair failed.

**Likely causes.** A startup race between the agent and zebra. The agent enslaves
`veth-provider` into the VRF and assigns its address; the kernel does not re-emit
`RTM_NEWADDR` when an interface changes VRF, so a zebra that records the address
and then processes the enslavement ends up holding the interface without its
prefix and never re-learns it on its own.

**Diagnosis.** The signature is a VRF that knows the interface but not its own
prefix:

```bash
vtysh -c 'show ip route vrf vrf-provider connected'   # 169.254.0.0/30 absent
vtysh -c 'show ip route vrf vrf-provider static'      # no S>* routes at all
vtysh -c 'show running-config' | grep 'ip route'      # …yet all /32s configured
ip route show table 100 | grep veth-provider          # …and the kernel has it
```

`show bgp vrf vrf-provider ipv4 summary` on the upstream peer shows `PfxRcd 0`
from this gateway for the duration.

**Remediation.** None normally required — the agent repairs this itself, at most
once a minute for as long as the condition lasts, and the repair restores the
per-network leak routes it disturbs. Investigate if the alert repeats: a
next-hop that goes unresolvable again after a successful repair is no longer the
startup race, and `journalctl -u frr` around the repair will show whether zebra
is restarting or losing the interface for some other reason.

## Alert: OVNNetworkAgentNBDisconnected

Fires on `ovn_connection_state{database="nb"} == 0` for 2m.

**Meaning.** The agent has lost its connection to the OVN Northbound database.

**Likely causes.** `ovn-northd` / the NB `ovsdb-server` is down or failing over,
a network partition to the NB endpoint, or TLS/authentication problems.

**Diagnosis.** The agent keeps retrying by contract — look for repeated
`failed to connect to OVN, retrying` lines in the journal (each logs `retry_in`).
Verify NB reachability from this node with `ovn-nbctl --db=<nb-remote> show`.

**Remediation.** While NB is unreachable, all agent-managed NB writes stall —
gatewayless default routes and static MAC bindings are not updated. Restore NB
availability; the agent reconnects and reconciles automatically once it is back,
with no restart required.

## Alert: OVNNetworkAgentRouteFlapping

Fires on `rate(route_readds_total[10m]) > 0`.

**Meaning.** Routes are being re-added by post-change verification, i.e. they
are disappearing and being reinstalled. This is the same underlying condition
as [`OVNNetworkAgentRouteInstability`](#alert-ovnnetworkagentrouteinstability),
caught earlier: any re-add at all raises the rate, before it has persisted for
three consecutive cycles.

**Likely causes.** Intermittent `vtysh` races, an FRR reload, or another process
occasionally clearing a route.

**Diagnosis.** Split the flapping by plane with the `plane` label
(`route_readds_total{plane="frr"}` vs `{plane="kernel"}`) to see whether FRR or
the kernel is losing routes, then follow the diagnosis for
[`OVNNetworkAgentRouteInstability`](#alert-ovnnetworkagentrouteinstability).

**Remediation.** Same as route instability: identify the competing writer. Brief
flapping around an FRR reload or a deploy is expected; sustained flapping is
not.

## Alert: OVNNetworkAgentInactiveRoutes

Fires on `inactive_routes > 0` for 2m.

**Meaning.** One or more desired FIP `/32`s exist as FRR static routes but are
not selected/installed, so they are **not advertised via BGP** and the FIPs are
unreachable from outside. Re-adding does not help — the route already exists;
the next-hop is the fault.

**Likely causes.** The static route's next-hop (`veth_nexthop` in the VRF) is
unresolvable — the veth pair is missing or down, or the VRF is misconfigured —
so FRR keeps the route but never installs or advertises it.

A port-forward VIP on a gateway without locally active routers is *not* a cause:
such a VIP is [dormant](../explanation/port-forwarding#dormant-vips-on-a-gateway-without-local-routers)
and gets no route at all, so it cannot contribute to this alert.

**Diagnosis.** The agent logs `FRR static routes are configured but inactive —
these FIPs are not advertised via BGP` with the affected `ips` and `vrf`.
Confirm with step 5 of [Agent up but no routes
appear](#agent-up-but-no-routes-appear) and check that the veth-leak plumbing
(step 6, table `200`) and the next-hop are in place.

**Remediation.** Fix the next-hop resolution: bring up the veth pair, verify the
VRF membership, and confirm the nexthop is reachable in the VRF. Once the
next-hop resolves, FRR selects and advertises the routes on the next cycle.

## Alert: OVNNetworkAgentHairpinFlowsMissing

Fires on `hairpin_flows_installed < hairpin_flows_desired` for 1m.

**Meaning.** The provider bridge carries fewer `cookie=0x998` hairpin flows than
the agent computed targets for. The FIPs behind the missing flows are
unreachable **from workloads on this same chassis**, while clients elsewhere
still reach them over the physical network — the asymmetry that makes this
class of fault hard to recognise from a user report.

`hairpin_flows_installed` is read from a dump taken at the start of a reconcile,
before the agent touches anything, so a one-cycle deficit is the normal shape of
a flow the agent is about to heal. The `for: 1m` is what separates that from a
flow the agent cannot install at all.

**Likely causes.** An `ovs-ofctl add-flow` that keeps failing (check
`ovs_flow_apply_errors_total{plane="hairpin"}`); ovn-controller recreating the
provider-bridge patch port, which drops every flow bound to the old OpenFlow
port number; or a hairpin target the agent cannot render — an unparseable
external IP on a NAT row, or a localnet segment with no patch port on the
bridge, both of which it counts as desired and warns about.

**Diagnosis.** Compare the two gauges against the bridge:

```bash
ovs-ofctl --no-stats dump-flows br-ex cookie=0x998/-1
```

Then read the agent log for `skipping hairpin flow for invalid IP`, `no segment
binding for hairpin IP, skipping`, and `skipping OVS hairpin flow reconcile:
segment bindings not yet discovered`. A non-zero
`ovs_flow_apply_errors_total{plane="hairpin"}` names the failing mutation in the
log line right before it.

**Remediation.** A segment with no binding needs its localnet port back on the
provider bridge (`ovs-vsctl list-ports br-ex` and the `ovn-localnet-port`
`external_ids` on each patch port). A NAT row with an unparseable external IP is
fixed in OVN NB. When neither applies, the flows heal on the next reconcile
once `ovs-ofctl` stops rejecting them.

## Alert: OVNNetworkAgentVRFDefaultRouteMissing

Fires on `vrf_default_route_present == 0` together with `announced_vips > 0` or
`local_routers > 0`, for 5m.

**Meaning.** The provider VRF holds no default route, so traffic to any
destination the VRF does not already route is dropped inside it. The agent
never installs this route — it comes from the fabric — but several of its data
paths depend on it.

The path that breaks first is a port-forward VIP whose gateway port lives
elsewhere. The chassis holding a router's chassisredirect port is where OVN
client traffic leaves, and the chassis holding the VIP's DNAT rules is where
that VIP is answered; on a fleet where only some nodes carry the VIP
configuration those are different nodes. The request then has to leave the
owner's VRF towards the peer that announces the VIP, and the reply has to leave
the announcing node's VRF towards a client behind a FIP the owner announces.
Neither destination is connected or redistributed, so without a default both
are dropped while the VIP address, the DNAT ruleset and the BGP announce all
look healthy.

**Likely causes.** The fabric stopped originating the default (a peer
reconfigured, a session down), the BGP session in the VRF is down, or the VRF
was never given one. On a node whose only peer withdrew the default, the FIP
`/32`s it learns still arrive, so the session looks alive at a glance.

**Diagnosis.** Ask the kernel first — that is what the gauge reads — then BGP:

```bash
ip route show vrf vrf-provider default          # empty is the alert's condition
vtysh -c 'show bgp vrf vrf-provider ipv4 summary'
vtysh -c 'show bgp vrf vrf-provider ipv4 0.0.0.0/0'
```

When the VIP is the symptom, confirm the split first: the chassis in
`ovn-sbctl find Port_Binding logical_port=cr-<router>-<net>` is the one that
must reach the VIP, and it is a different node from the one whose
`nft list table ip ovn-network-agent` holds the DNAT rule.

**Remediation.** Restore the default in the fabric — on an FRR upstream that is
`neighbor <gateway> default-originate` in the VRF's `address-family ipv4
unicast`. Routes covering every client network work equally well; the agent
checks for a default because that is what a provider VRF normally receives.
Giving every gateway the same `port_forwards` configuration removes the split
that makes this visible, but not the dependency: OVN client egress to anything
outside the VRF's routed networks still needs the route.

## Alert: OVNNetworkAgentSlowFailoverAnnounce

Fires on
`histogram_quantile(0.95, rate(failover_announce_seconds_bucket[1h])) > 2`.

**Meaning.** The p95 time from observing a chassisredirect change to completing
the BGP announce of the takeover FIP routes exceeds the ~2s failover budget.
Slow announces widen the packet-loss window during gateway failover.

**Likely causes.** `vtysh`/FRR latency during the announce, slow OVSDB reads on
the takeover reconcile, or an overloaded node.

**Diagnosis.** Correlate `failover_announce_seconds` spikes with reconcile
duration (`reconcile_duration_seconds`) and with FRR/`vtysh` responsiveness on
the node during the failover window.

**Remediation.** Reduce contention on the takeover node and on FRR; ensure
`vtysh` responds promptly. Persistent breaches point at an undersized or
overloaded gateway node.

## Alert: OVNNetworkAgentSlowReconcile

Fires on
`histogram_quantile(0.95, rate(reconcile_duration_seconds_bucket[5m])) > 5`.

**Meaning.** The p95 reconcile cycle takes longer than 5 seconds. Slow
reconciles delay the agent's reaction to OVN changes.

**Likely causes.** High `vtysh`/FRR or OVSDB latency, or simply a large cloud —
many routers, FIPs, and networks make each cycle do more work.

**Diagnosis.** Check `desired_ips`, `local_routers`, and `effective_networks`
to gauge the workload size, and watch for slow OVSDB or `vtysh` calls in the
journal at debug level.

**Remediation.** Address the slow dependency (OVSDB/FRR latency). If the cause
is genuine scale, this may be the expected steady state — raise the alert
threshold to match the cloud rather than chasing a non-problem.

## Drain outcomes

The `drain_total` counter is labelled by `outcome`. The failure and no-op
outcomes are the ones to watch:

- `drain_total{outcome="timeout"}` — the drain did not finish migrating the
  gateways away within `drain_timeout`; the agent proceeded with shutdown
  anyway. Traffic may have blackholed briefly during the reboot.
- `drain_total{outcome="error"}` — the drain failed (e.g. an NB write to lower
  `Gateway_Chassis` priority did not go through).
- `drain_total{outcome="noop"}` — there was nothing to drain: no
  `Gateway_Chassis` rows with priority > 0 for this chassis. This is **normal**
  on a standby that was already at priority 0, but **suspicious** on a node you
  believe is an active gateway (it suggests a chassis-name mismatch or a
  previous drain that was never restored).

**Diagnosis.** Read the `drain:` lines from the shutdown in the journal:

```bash
journalctl -u ovn-network-agent | grep 'drain:'
```

The informative ones are `drain: no gateway chassis entries to drain on this
chassis` (the noop path — it logs `local_chassis_name` and `cache_entries` so
you can spot a name mismatch), `drain: gateway chassis priority lowered`,
`drain: waiting for gateway migration`, and `drain: timeout exceeded, proceeding
with shutdown`. Cross-check the priorities directly:

```bash
ovn-nbctl list Gateway_Chassis
```

For a noop on a supposedly active gateway, also confirm the standby chassis is
healthy enough to take over.

**Remediation.** For timeouts, verify the standby chassis can take over and, if
migration is genuinely slow, raise `drain_timeout`; tune `drain_settle_delay`
for the post-readiness safety margin. For errors, treat it as an NB write
failure (see below). For the conceptual model and the shutdown sequence, see
[Gateway drain mode](../explanation/gateway-drain); to change the behaviour see
[Configure gateway drain](../guides/gateway-drain).

## Stale-chassis cleanup errors

`stale_chassis_cleanup_total{outcome="error"}` counts failed attempts to remove
OVN NB entries belonging to a chassis that has disappeared from the SB Chassis
table.

**Likely causes.** An NB write failure or lost NB connectivity at the moment the
cleanup ran.

**Diagnosis.** The agent logs `failed to clean up stale chassis entries` with the
error. Watch the `missing_chassis` gauge: a non-zero value that never returns to
zero means the cleanup keeps failing for the same chassis.

**Remediation.** Restore NB reachability (see
[`OVNNetworkAgentNBDisconnected`](#alert-ovnnetworkagentnbdisconnected)). No
manual intervention is needed for the cleanup itself — it retries on the next
reconcile cycle once NB is writable again.

## Actionable error-level log messages

Not every `error`-level line demands action: many reconcile steps are
best-effort and the next cycle retries them automatically. This table separates
the messages that need you from the ones that heal themselves. Message text is
quoted by its prefix, exactly as it appears in the journal.

| Message | What it means | Action |
|---------|---------------|--------|
| `persistent route instability detected…` | Managed routes were re-added for ≥3 consecutive cycles. | **Act.** Find the competing route writer — see [OVNNetworkAgentRouteInstability](#alert-ovnnetworkagentrouteinstability). |
| `FRR static routes are configured but inactive…` | FIP `/32`s exist in FRR but are not advertised via BGP. | **Act.** Fix the next-hop — see [OVNNetworkAgentInactiveRoutes](#alert-ovnnetworkagentinactiveroutes). |
| `drain failed` | The shutdown drain returned an error; the agent still proceeds with shutdown. | **Act.** Check `Gateway_Chassis` priorities and standby health — see [Drain outcomes](#drain-outcomes). |
| `failed to start metrics endpoint` | The metrics listener could not bind; the process exits. | **Act.** Free the `metrics_listen` address or fix permissions, then restart. |
| `failed to create agent` / `agent exited with error` | Fatal startup or run error; the process exits. | **Act.** Read the accompanying `error` and the journal. |
| `failed to connect to OVN, retrying` | NB/SB endpoint unreachable at connect time. | **Self-healing.** The agent retries every `retry_in`; act only if it never connects — see [OVNNetworkAgentNBDisconnected](#alert-ovnnetworkagentnbdisconnected). |
| `failed to ensure gateway routing` | An NB write for a managed default route or MAC binding failed this cycle. | **Self-healing** if transient; the next reconcile retries. Investigate if it persists (NB connectivity). |
| `failed to ensure OVS flows` / `failed to reconcile OVS hairpin flows` | Programming OVS flows failed this cycle. | **Self-healing**; retried next cycle. Investigate if it persists (OVS reachable via `ovs-wrapper`?). |
| `failed to reconcile FRR prefix-list` | Updating the BGP outbound filter failed this cycle. | **Self-healing**; retried next cycle. Investigate if it persists (`vtysh`/FRR reachable?). |
| `failed to reconcile veth leak networks` | Programming the veth-leak routes/rules failed this cycle. | **Self-healing**; retried next cycle. |
| `failed to reconcile port forwarding` | Programming the DNAT rules failed this cycle. | **Self-healing**; retried next cycle. |
| `failed to clean up stale chassis entries` | Removing a departed chassis's NB entries failed. | **Self-healing**; retried next cycle — see [Stale-chassis cleanup errors](#stale-chassis-cleanup-errors). |
