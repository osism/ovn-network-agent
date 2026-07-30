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

- Per recovery event, `from_inject_ms` (per probe) is downtime measured
  from fault injection; `from_restore_ms` is the part after the fault was
  lifted. The hold (`hold_ms` on the `decision` journal event, 0–45 s
  by action) is the fault window itself.
- **Downtime ≈ hold + ~1–3 s, per probe** means that traffic class never
  failed over and waited for the node to come back. That is the smell to
  chase: with three gateways, loss should end at failover (a few
  seconds), not at restore.
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

## 4. Known-good baseline (2026-07-30, runs 30288259170..30518214683)

- Overall probe loss 5.13 % pooled; best night 0.4–1.9 % per profile.
- Fast and fine: all pause/churn/drop actions converge in ~100–300 ms
  with no downtime.
- Standing offenders (each ends ~1–3 s after restore, i.e. no failover):
  `gateway-kill`/`double-failover` up to 42 s on VLAN+PF probes,
  `controller-restart` ~15 s, `agent-terminate` ~19 s,
  `upstream-bgp-restart` ~15–19 s on every probe,
  `config-flip`/`gateway-restart` 6–14 s.
- A consistent ~2.4–3.2 s residual follows nearly every restore.

Regressions are deviations from this shape, not from zero.

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
