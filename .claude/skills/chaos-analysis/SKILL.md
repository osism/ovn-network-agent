---
name: chaos-analysis
description: Analyze recent E2E Chaos runs for packet loss and downtime, attribute both to fault actions, compare nights, and derive concrete smoothness measures. Use when asked to analyze chaos/E2E runs, investigate packet loss or failover downtime, or check whether a change made the agent smoother.
---

# Chaos run smoothness analysis

Aggregate the chaos-run records of several E2E Chaos runs, attribute
packet loss and downtime to fault actions, and turn the worst offenders
into concrete, issue-ready measures.

## 1. Collect the runs

```sh
gh run list --workflow "E2E Chaos" --limit 10 \
  --json databaseId,conclusion,createdAt,event
```

Pick the runs to compare (default: the last 4 with artifacts — artifact
retention is 7 days). Download each into its own directory:

```sh
for run in <id...>; do
  gh run download "$run" --repo osism/ovn-network-agent --dir e2e-runs/"$run" &
done; wait
```

Nightly runs hold one artifact per profile (6); a dispatch run holds one.

## 2. Aggregate

```sh
python3 .claude/skills/chaos-analysis/analyze.py e2e-runs/* > report.md      # all runs pooled
python3 .claude/skills/chaos-analysis/analyze.py e2e-runs/<one-run>          # one night alone
```

`--format json` emits the same aggregation for scripted drill-downs.
Always ALSO run the per-night breakdown for each run: the pooled table
mixes seeds, and a fix shows up as one night improving, not as the pool
improving. Correlate nights with `git log` — nightlies run ~06:00 UTC,
so a fix merged in the evening first shows in the next morning's run.

## 3. Read the numbers — semantics that matter

- Per recovery event, `down_ms` (per probe) is the summed length of the
  probe's red windows between the inject and the convergence, and
  `down_windows` is their count. The hold (`hold_ms` on the `decision`
  journal event, 0–45 s by action) is the fault window itself.
- `from_restore_ms` is the part of `down_ms` after the restore anchor.
  The anchor is when the restore command returned, which is later than
  the `restore` journal line by the restore's own duration. Records
  written before `down_ms` existed carry the older restore-to-last-recovery
  span in `from_restore_ms`; `analyze.py` leaves those out of `residual`
  and says how many it left out.
- `from_inject_ms` is the older span from the inject to the probe's last
  recovery. It stays in the record for old readers and is in no table:
  it reads a 1.2 s failover followed by a 90 ms blink at restore as 41 s.
- A probe that stayed dark for the hold shows `down_ms ≈ hold_ms` with
  `down_windows` 1. A failover shows a short window plus, often, a second
  short window at restore.
- **`from_restore_ms` (residual)** is the tail the recovery path owns —
  the agent's reaction plus BGP re-establishment after the node returns.
- Probe classes: `fip-vm*` = FIPs on the flat (Geneve-backed) provider
  network, `fip-vlan10*` = FIPs on VLAN provider networks, `pf-vip` =
  DNAT port-forward VIP, `api-vip` = LB VIP. Which classes go dark in an
  event tells you which networks' CR ports the target chassis owned.
- `check-error` events with `ovn-sbctl --timeout=5` during `sb-pause`
  holds are harness-side sweep noise, not agent regressions.
- Actions whose median worst downtime is 0 with occasional small `after`
  loss windows (route drops, churn, pauses) are healthy.
- Planned restarts (`agent-terminate`, `gateway-restart`, `config-flip`)
  are read from the `drain-everywhere` profile's run report, whose
  Planned restarts section splits them by drain and by how a flip landed
  (reload or restart). `analyze.py`'s per-action rows pool drained and
  undrained events. The profile probes the VLAN FIPs and `cross-fip`
  too: their routers have a standby chassis, so loss on them during a
  drained restart is a finding.

## 4. Known-good baseline (2026-09-26..29, runs 36230357161, 36308907967, 36405972578, 36551938602 and the dispatch runs 36596597161, 36608325274, 36608338899, 36608352426, 36608365961, 36608379365, 36608391793; 35 records)

In `down_ms` terms, computed from those records' journals with the
window sum `downtime_from_journal` implements. `median worst probe` and
`max worst probe` are taken over each event's worst probe; `total` sums
every probe's `down_ms` over all events.

| action | events | with downtime | median worst probe | max worst probe | total | windows |
| --- | --- | --- | --- | --- | --- | --- |
| double-failover | 18 | 16 | 37.5 s | 41.2 s | 2765.1 s | 82 |
| gateway-kill | 29 | 21 | 35.2 s | 46.4 s | 1358.6 s | 84 |
| agent-terminate | 24 | 17 | 18.6 s | 28.9 s | 1114.5 s | 71 |
| upstream-bgp-restart | 23 | 23 | 12.4 s | 19.1 s | 1017.7 s | 78 |
| controller-restart | 19 | 10 | 0.1 s | 19.4 s | 277.1 s | 33 |
| gateway-restart | 26 | 18 | 7.6 s | 9.1 s | 267.2 s | 64 |
| frr-restart | 28 | 11 | 0.0 s | 5.7 s | 140.6 s | 30 |
| config-flip | 25 | 8 | 0.0 s | 9.1 s | 116.9 s | 38 |
| frr-route-drop | 16 | 2 | 0.0 s | 11.6 s | 16.9 s | 2 |

Every other action (northd-pause, mgmt-loss, mgmt-delay, nb-pause,
chassis-delete, lb-vip-churn, fip-churn, kernel-route-drop,
ovs-flow-drop, priority-flip, nft-flush, sb-pause) had no downtime in
those runs. Total 7074 s, against 8563 s by `from_inject_ms`.

Most of the top four rows is loss the lab causes by design, tracked in
#236 (a fault on the workload host `gateway-3` darkened every probe; the
VLAN routers on `gateway-1` and lr1 on `gateway-2` had no standby
chassis; `pf-vip`'s upstream route is re-pointed only after the
restore), so the ranking is not an agent ranking. The table predates
the move of the workloads from `gateway-3` to the compute chassis
`compute-1` (issue #280): its rows include the workload-host loss of
every fault on `gateway-3`, so runs recorded after the move are not
comparable to it until the baseline is recomputed. The table also
predates the standby chassis on the VLAN routers and lr1 (issue #281),
so its single-chassis rows are not comparable to later runs. A
SIGKILLed lr0 owner fails over in about 1.2 to 3 s on the flat FIPs
(nightly 36551938602, `everything-on`, tick 8: `fip-vm1` 1.3 s in 2
windows) while `pf-vip` and the VLAN FIPs stayed dark for the hold in
that run.

Regressions are deviations from this table on the flat-FIP probes and
on the `frr-restart` and `upstream-bgp-restart` rows.

## 5. Derive measures and file issues

For each offender, name the mechanism before proposing a fix: replay it
first (`make e2e-chaos CHAOS_FLAGS="-seed <seed> -profile <profile>
-duration 10m"` — seed and profile are in the report/summary.json) when
the mechanism is unclear. Then propose one measure per root cause, each
with: the measured cost (seconds of downtime × frequency), the affected
probe classes, the suspected code/config surface, and the replay line.

File issues per the user's convention: a few independent, fully-detailed
issues (not one plan), `gh issue create` with measured evidence and
acceptance criteria phrased against these metrics (e.g. "worst-probe
downtime for gateway-kill on the owner drops below 5 s").
