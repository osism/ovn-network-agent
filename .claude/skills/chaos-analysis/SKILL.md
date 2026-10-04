---
name: chaos-analysis
description: Analyze E2E Chaos runs for packet loss and downtime and look for optimisations. Attributes loss to fault actions and to the phase of the fault, separates the loss the lab causes by design, ranks what is left as optimisation candidates, drills into single events, and compares runs before and after a change. Use when asked to analyze chaos/E2E runs, investigate packet loss or failover downtime, find improvements or optimisations in failover and recovery, or check whether a change made the agent smoother.
---

# Chaos run analysis: packet loss and what to optimise

Aggregate the chaos-run records of several E2E Chaos runs, cut the loss
down to what the agent and its deployment can change, name the mechanism
behind each candidate, and turn the worst ones into issue-ready measures.
The harness itself is described in `docs/contributing/e2e-tests.md`
(Chaos runner, Configuration profiles).

## 1. Collect the runs

```sh
gh run list --workflow "E2E Chaos" --limit 40 \
  --json databaseId,conclusion,createdAt,event,headSha \
  --jq '.[] | [.databaseId, .createdAt, .event, .conclusion, .headSha[0:7]] | @tsv'
```

Pool only runs of one build: group by `headSha`, and take the nightly
(7 records, one per profile) plus the dispatch runs (one record each) of
the commit under test. Artifact retention is 7 days. Download into
`chaos-artifacts/` (ignored by git), one directory per run:

```sh
printf '%s\n' <id...> | xargs -P 6 -I{} \
  gh run download {} --repo osism/ovn-network-agent --dir chaos-artifacts/runs/{}
```

A candidate needs events to stand on: one run injects each action about
once. With fewer than about five events of an action, say so instead of
ranking it. `make e2e-chaos-random CHAOS_RANDOM_FLAGS="-n 20"` dispatches
20 more runs of the current branch on the self-hosted runners; ask
before dispatching.

A run that did not pass is triaged on its own first (Triaging a failed
run in `docs/contributing/e2e-tests.md`): its loss belongs to the
violation, not to the smoothness ranking.

## 2. Aggregate

```sh
A=.claude/skills/chaos-analysis/analyze.py
python3 $A chaos-artifacts/runs/* > chaos-artifacts/report.md   # all runs pooled
python3 $A chaos-artifacts/runs/<one-run>                       # one run alone
python3 $A chaos-artifacts/runs/* --format json                 # the same, for scripted drill-downs
```

The report opens with the **open loss** table, which is the list of
optimisation candidates, and the loss the lab causes by design. The
per-action tables follow: `down_ms` per action, the worst events, the journal
loss windows and the loss per probe.

## 3. Read the numbers

### What a probe measures

- A probe runs once a second. `ping` waits 1 s for its reply, the HTTP
  and TCP probes 3 s. A probe goes red when its command returns and green
  when the next one does, and after a timeout the next one starts at once.
- So a window understates the outage. A `ping` window stands for an
  outage about 1 s longer (0 to 2 s). A measured 2.3 s failover is 3.3 s
  of lost traffic.
- A **blip** is a window under 500 ms: exactly one lost probe. Its length
  says nothing. For an outage shorter than a second, the share of events
  with a blip estimates its length: blips in 3 of 5 events is about 0.6 s.
- The HTTP and TCP probes (`pf-vip`, `api-vip`, `hairpin-vip`) ride out an
  outage shorter than a SYN retransmission and stamp a longer one up to
  3 s late. Use the `ping` probes of the same event for the timing.
- Packet counts (`sent`, `lost`) are per probe for the whole run, in the
  Runs and Loss by probe tables. They follow the seed: whether it drew a
  double-failover or an upstream restart decides a run's loss percentage.
  Never read a trend off pooled loss.

### Phases

`analyze.py` cuts every loss window at its fault's restore:

| phase | the window | reads as |
| --- | --- | --- |
| `failover` | opened and closed during the hold | detection plus takeover; the standby works |
| `dark` | was still open when the restore began | nothing took over, or the hold was shorter than the takeover |
| `tail` | the rest of a dark window after the restore returned | the recovery path: BGP re-establishment, re-binding |
| `restore` | opened after the restore began | loss the return causes; for a fault with no hold, the reaction to the fault |
| `after` | opened after every probe was confirmed green | loss with no fault to blame |

A fault with no hold (`frr-restart`, the drift actions, the churn) has
its restore right behind the inject. A record written since #292 has the
reaction to such a fault in `restore`: the engine declares convergence
only once every probe had two green samples started after the restore.
For a restart (`gateway-restart`, a restarting `config-flip`) the inject
is the stop and the restore the start, so the handover lands in `dark`
or `restore`. Older records converged before a probe could go red. In
them, and only in them, an `after` window that opens within about 2 s of
the convergence is that fault's own loss, which no recovery budget
covered. A record's build is its run's `headSha` (section 1).

### Columns of the open loss table

- `events hit` is the events with loss in that phase over all injected
  faults of the action. An event's downtime is its worst probe's, so a
  profile with seven probes weighs like one with one.
- `per event` spreads the downtime over every event, hit or not. It is
  the expected cost of injecting the fault once, and the number to
  compare across builds.
- `median hit` and `max hit` describe the events that were hit. A wide
  gap between them is two mechanisms in one row: look at both.
- `slow cadence` is the median of the hit events whose target reconciled
  every 15 s instead of 5 s (a `cadence-toggle` flip, or `gateway-3`
  under `heterogeneous`). If it is about three times the fast median,
  the repair waits for the periodic reconcile. Production's default
  `reconcile_interval` is 60 s, twelve times the lab's.
- Action labels carry how the fault landed: `(drained)` for a planned
  restart with the drain on, `(reload)`, `(restart)` or `(rejected)` for
  a `config-flip`. A drained restart is expected to be hitless, so every
  `(drained)` row is a finding.
- `probes`: `fip-vm*` are FIPs on the flat provider network behind `lr0`,
  `fip-vlan10*` FIPs behind the VLAN routers, `cross-fip` leaves OVN on
  `lr1`'s chassis and enters on `lr0`'s, `hairpin-*` stay on one chassis,
  `pf-vip` is an OVN load balancer the runner routes, `api-vip` the
  agent's own DNAT. Which probes went dark tells which routers the target
  owned.

### Loss the lab causes by design

`by_design` in `analyze.py` takes four classes out of the candidates:
the hold of an `upstream-bgp-restart` (one upstream router), the routers
a `double-failover` leaves without a chassis (the VLAN routers for
`gateway-1` + `gateway-2`, `lr1` for `gateway-2` + `gateway-3`), the API
VIP when `heterogeneous` loses both gateways that carry it, and `pf-vip`
windows that a runner re-point ends. Their `tail` stays a candidate. The
flat FIPs of the same event carry the agent's share of a `pf-vip` loss.

The rules mirror `test/e2e/chaos/state.go` and `profiles.go`. When the
lab's topology, profiles or probe cadence change, update the constants
at the top of `analyze.py` and its tests in the same commit.

### Other semantics

- `down_ms` (per probe, per recovery) is the summed red windows between
  the inject and the convergence, `down_windows` their count,
  `from_restore_ms` the part after the restore returned. `from_inject_ms`
  is the older inject-to-last-recovery span and is in no table.
- `converged_ms` is the time from the restore until every probe had two
  green samples started after it, so it lies about 1 to 2 s behind the
  last probe's recovery. In a record older than #292 it is the engine's
  first poll for a fault with no hold: `converged` only asked whether the
  node was back and every probe green, and a probe needs a second to go
  red, so `ovs-flow-drop` "converged" in 0.4 s and then lost `cross-fip`
  for seconds. Read the `after` rows of such a record, never its
  `converged_ms`, for how long a repair took.
- `check-error` events with `ovn-sbctl --timeout=5` during `sb-pause`
  holds are sweep noise of the harness (#239).
- The owner of `cr-lr0-public` is journaled only by profiles with the
  port-forward layer (`cr_owner` on `converged`, `vip-repoint`) and by
  the fault trace of a `double-failover`. Elsewhere infer it from the
  probes that went dark.
- `cr-owner` and `upstream-path` events are the fault trace of a
  `double-failover`; the run report renders them per tick under Fault
  traces. The rows with `detail` `baseline` say which chassis owned each
  chassisredirect port and which gateways the upstream forwarded each
  prefix over before the inject. A `baseline` row journaled after the
  `inject` is the exception: the read before the inject failed (see the
  `check-error` that starts with `fault trace:`), and the row is the
  state at its own time, which the report shows instead of `before`.
  Every later row is a change with `from` and `to`: `unbound` is a port
  no chassis owns, `none` a prefix with no selected gateway path,
  `absent` an object missing from the reading. The time from a port's
  `unbound` row to its next owner is how long OVN left the port
  unclaimed. The time from there to the prefix's `upstream-path` row is
  the agent's reconcile plus BGP.

## 4. Find the mechanism

Work the open loss table from the top. For each row worth more than a
blip, read the worst event and one typical event:

```sh
python3 $A chaos-artifacts/runs/* --timeline <run id>:<tick> [--context 20]
```

The timeline lists the journal around the fault with offsets from the
inject: when each probe went down and for how long, when the restore
began and returned, the re-points, the config flips, the faults that
came before it, the target's reconcile cadence and the replay lines.
The `worst event` column gives the record and tick.

Name the mechanism before proposing anything. The journal shows when
traffic stopped and came back, not why: a signature below is where to
start, and stays a hypothesis until the code or a replay confirms it.

| signature | likely mechanism | where to look |
| --- | --- | --- |
| downtime near the target's `reconcile_interval`, tripling at slow cadence | the repair waits for the periodic reconcile | the callers of `triggerReconcile` in `agent.go`; the route watch (`route_watch.go`) covers routes, the OVS flow watch (`flow_watch.go`) the hairpin and MAC-tweak flows and the FRR watch (`frr_watch.go`) an FRR restart, no watch covers nftables |
| `restore` loss on `cross-fip` behind `ovs-flow-drop` (`after` in records before #292) | the OVS flow watch did not repair the flows: its `flow_drift` is 0, or absent as in every record before #291, and the periodic reconcile put them back | the `OVS flow watch` and `OVS flow monitor` lines of the target's agent log; `flow_watch.go`, `ovs.go`, #291 |
| about 4.5 s `restore` after `frr-restart` on the announcing gateway, ending 6 to 7 s after the inject whatever the phase of the reconcile ticker | FRR's own restart plus the BGP re-establishment the restore forces with `no router bgp` | `restoreGatewayFRR` and `configureGatewayBGP` in `test/e2e/chaos/`, #238 |
| one 14.3 s `frr-restart` on a gateway at slow cadence (run 36877177662 tick 11) | the repair waited for the periodic reconcile: the agent never saves its statics and prefix-list entries, so the restarted FRR came back without them, and before #293 nothing triggered a reconcile once FRR was back (the route watch's reconcile ran while FRR stopped). Named from the code path in #293, which predicts that the repair in a replay's agent log is a `reconciling` line with `trigger=periodic` | the `FRR restart detected, reconciling` and `reconciling` lines of the target's agent log; `frr_watch.go`, #293 |
| 2 to 3.5 s `failover` after a kill, an undrained terminate or a double-failover on the owner | detection plus the takeover on the standby | OVN's BFD on the tunnels, #128, #130 |
| 2.5 to 3.2 s `tail` on every probe after `upstream-bgp-restart` | BGP re-establishment on FRR default timers | the FRR config in `test/e2e/bootstrap.sh` and `configureGatewayBGP` (`test/e2e/chaos/lab.go`), #237, #238 |
| loss on a `(drained)` restart | the handover is not hitless | `docs/explanation/gateway-drain.md`, `ovn_gateway.go`; #236 (closed) made planned restarts drain, #284 |
| one blip per probe about 1 s after a chassis returns (`controller-restart` `restore` rows, `after` in records before #292) | not named yet | #235 |
| `pf-vip` longer than the flat FIPs of the same event | the runner's owner poll and re-point | harness, not the agent |

Confirming needs evidence the journal does not hold: read the code path,
or replay the event. The lab needs a Linux host with the `openvswitch`,
`vrf` and `sch_netem` modules, so from a Mac dispatch the replay (the
`gh workflow run` line is in the timeline). Every run uploads
`lab-state/` (runs before #293 only when they did not pass), with the
agent logs under `agent/<gateway>.log`. Each `reconciling` line names its
`trigger` (`startup`, `event`, `periodic`), and the line before an
`event` cycle names the watch that fired.

Before proposing, search the issues, open and closed: `gh issue list
--state all --search "<keyword>"`. New evidence for a known mechanism
goes onto its issue.

## 5. Report the candidates

Give the user a ranked list, most expensive first by `per event` times
how often the fault happens in production (a planned restart on every
rollout, a kill rarely). Per candidate:

- the measured cost: events hit, per event, median and max hit, plus 1 s
  for the probe floor, and the production figure where the loss scales
  with `reconcile_interval`
- the probes and so the traffic classes it hits
- the mechanism, with the timeline lines that show it, or "hypothesis"
  when only the signature matches
- the proposed change and whose it is: the agent, the deployment (FRR,
  BGP and BFD settings, documented defaults), or the harness (a
  measurement gap, a too-generous budget)
- the row that has to move in step 6, and the issue it belongs to

Keep the by-design total and the candidates the data was too thin for in
the report, each in a line.

## 6. Check a change

```sh
python3 $A chaos-artifacts/after/* --baseline chaos-artifacts/before/*
```

appends a baseline → new table per action and phase, sorted by the
change in `per event`. Read `events hit` first: a row with two events on
either side proves nothing. A fix shows as its own row dropping while
the others hold. The 2026-10-01 route watch reads like this (20 records
each side):

| action | phase | events hit | per event | median hit |
| --- | --- | --- | --- | --- |
| kernel-route-drop | after | 4/4 → 3/5 | 2.3 s → 57 ms | 2.3 s → 96 ms |
| frr-route-drop | after | 6/6 → 4/6 | 2.3 s → 67 ms | 2.2 s → 94 ms |
| upstream-bgp-restart | tail | 10/10 → 9/9 | 2.8 s → 2.9 s | 2.8 s → 2.9 s |

## 7. Baseline (2026-10-01, commit 7d551c7, dispatch runs 36877145078 to 36877435520; 20 records, all passed)

This baseline predates the probe confirmation (#292). Its `after` rows
for `ovs-flow-drop`, the route drops, `priority-flip`, `chassis-delete`
and `controller-restart` appear under `restore` in later records, and
its `converged_ms` medians are not comparable with theirs. Recomputing
it needs 20 runs of a build with the confirmation.

Probe loss 1107/69078 (1.60 %). Probe-down time 1066 s: 646 s by design
(upstream hold 314 s, unbound routers 230 s, API VIP pair 72 s, `pf-vip`
re-points 31 s), 420 s open. The open rows above 1 s of downtime:

| action | phase | events hit | per event | median hit | max hit | slow cadence |
| --- | --- | --- | --- | --- | --- | --- |
| frr-restart | restore | 8/18 | 2.6 s | 4.6 s | 14.3 s | 14.3 s (1) |
| ovs-flow-drop | after | 7/12 | 2.6 s | 2.3 s | 13.3 s | 12.3 s (2) |
| upstream-bgp-restart | tail | 9/9 | 2.9 s | 2.9 s | 3.2 s | — |
| agent-terminate | failover | 7/10 | 1.6 s | 2.3 s | 3.4 s | — |
| double-failover | failover | 4/10 | 1.1 s | 2.4 s | 3.5 s | 2.3 s (1) |
| gateway-restart | restore | 8/13 | 604 ms | 1.3 s | 2.3 s | — |
| config-flip (restart, drained) | dark | 1/4 | 573 ms | 2.3 s | 2.3 s | — |
| gateway-kill | failover | 2/7 | 473 ms | 1.7 s | 2.3 s | — |
| gateway-restart (drained) | failover | 2/4 | 313 ms | 626 ms | 1.2 s | 1.2 s (1) |
| config-flip (restart) | restore | 2/6 | 207 ms | 622 ms | 1.1 s | — |
| controller-restart | after | 10/26 | 156 ms | 116 ms | 1.2 s | 894 ms (3) |

Every other row is blips. No downtime at all: `nft-flush`, `fip-churn`,
`lb-vip-churn`, `nb-pause`, `sb-pause`, `northd-pause`, `mgmt-loss`,
`mgmt-delay`. The records follow the compute chassis (#280), the standby
chassis (#281), the owner poll (#282) and the route watch; runs older
than those are not comparable.

A regression is a row here whose `per event` rises with enough events
behind it, or a new row above 1 s.

## 8. File issues

Follow the user's convention: a few independent, fully detailed issues
rather than one plan. `gh issue create` with the measured evidence (the
open loss row, the timeline of the worst event, the replay line), the
mechanism, and acceptance criteria phrased against these numbers, e.g.
"`frr-restart` / `restore` per event below 1 s over at least 10 events,
and no rise at slow cadence".
