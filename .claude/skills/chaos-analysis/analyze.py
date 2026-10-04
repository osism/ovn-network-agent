#!/usr/bin/env python3
"""Aggregate chaos-run records across runs into a smoothness report.

Reads every summary.json (+ journal.jsonl next to it, when present) under
the given directories — the layout `gh run download` produces — and
answers the question the per-run report cannot: which fault actions cost
the most packet loss and downtime *across* runs, in which phase of the
fault, how much of it the lab causes by design, and where the recovery
budget headroom is thinnest.

Usage:
    analyze.py DIR [DIR ...] [--format md|json] [--top N]
    analyze.py DIR [DIR ...] --baseline DIR [DIR ...]
    analyze.py DIR [DIR ...] --timeline RECORD:TICK

Each DIR may be a single artifact directory (holding summary.json) or a
parent holding one subdirectory per profile/run. `--baseline` adds a
before/after table against a second set of runs. `--timeline` prints the
journal around one fault instead of the report; RECORD is any part of the
record's path that singles it out, e.g. the run id. Output goes to stdout.
"""

import argparse
import json
import statistics
import sys
from datetime import datetime, timedelta
from pathlib import Path

RECORD_SCHEMA = "chaos-run-record/v1"
# Mirrors probeInterval in test/e2e/chaos/probe.go: a record and its
# journal may disagree by up to one probe period.
PROBE_INTERVAL_MS = 1000
# A probe goes red when its command returns and green when the next one
# does, and the next one starts at once after a timeout. So one lost probe
# is a window of about one command's duration, well under this.
BLIP_MS = 500

# The routers a double-failover leaves without a chassis, by the pair it
# takes down. Mirrors the Gateway_Chassis rows of the chaos start state
# (test/e2e/chaos/state.go): the VLAN routers sit on gateway-1 and
# gateway-2, lr1 on gateway-2 and gateway-3, lr0 on all three.
UNBOUND_BY_PAIR = {
    frozenset(("gateway-1", "gateway-2")): {"fip-vlan101", "fip-vlan102"},
    frozenset(("gateway-2", "gateway-3")): {"cross-fip"},
}
# The profiles that put the API VIP on two gateways only
# (test/e2e/chaos/profiles.go).
VIP_PAIR_BY_PROFILE = {
    "heterogeneous": (frozenset(("gateway-1", "gateway-2")), {"api-vip", "hairpin-vip"}),
}
# pf-vip follows the owner of cr-lr0-public through the runner, not the
# agent. A probe times out after 3 s, so its window may open after the
# re-point that ends it.
REPOINT_LEAD = timedelta(seconds=5)
REPOINT_LAG = timedelta(seconds=1)

# reconcile_interval of the baked gateway config (test/e2e/gwnode-config.yaml)
# and the gateways a profile starts on another one (profiles.go). A
# `cadence-toggle` flip moves a gateway between the two (flips.go).
FAST_CADENCE = "5s"
START_CADENCE_BY_PROFILE = {"heterogeneous": {"gateway-3": "15s"}}


def parse_ts(s):
    # RFC3339Nano; Python handles up to microseconds, so trim the tail.
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    if "." in s:
        head, rest = s.split(".", 1)
        frac = rest[: rest.index("+")] if "+" in rest else rest
        tz = rest[len(frac):]
        s = f"{head}.{frac[:6].ljust(6, '0')}{tz}"
    return datetime.fromisoformat(s)


def find_records(roots):
    seen = set()
    for root in roots:
        root = Path(root)
        candidates = [root / "summary.json"] if (root / "summary.json").exists() else sorted(root.rglob("summary.json"))
        for path in candidates:
            if path in seen:
                continue
            seen.add(path)
            yield path


def load_record(path):
    rec = json.loads(path.read_text())
    if rec.get("schema") != RECORD_SCHEMA:
        print(f"skip {path}: schema {rec.get('schema')!r}", file=sys.stderr)
        return None, []
    events = []
    journal = path.parent / "journal.jsonl"
    if journal.exists():
        for line in journal.read_text().splitlines():
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                continue  # an interrupted run truncates its last line
    return rec, events


def fault_spans(events, run_end, profile=""):
    """The inject→converged span of every injected fault, keyed by tick in
    journal order. `restore` is when the restore began and `restored` when
    it had returned (the node went to converging). A fault that never
    converged spans to the run end. `cadence` is the reconcile_interval the
    target ran on when the fault hit it, and `mode` how a config-flip
    landed."""
    spans, last = {}, None
    cadence = dict(START_CADENCE_BY_PROFILE.get(profile, {}))
    for ev in events:
        kind = ev.get("event")
        if kind == "inject":
            last = spans[ev["tick"]] = {
                "tick": ev["tick"], "action": ev["action"], "target": ev.get("target", ""),
                "peer": ev.get("peer", ""), "drain": ev.get("drain"), "mode": "",
                "cadence": cadence.get(ev.get("target"), FAST_CADENCE),
                "inject": parse_ts(ev["ts"]), "restore": None, "restored": None, "end": None}
        elif kind == "config-flip":
            if last is not None and last["end"] is None:
                last["mode"] = "rejected" if ev.get("rejected") else ev.get("mode", "")
            if ev.get("flip") == "cadence-toggle" and not ev.get("rejected"):
                cadence[ev["target"]] = ev["to"]
        elif kind == "restore" and ev.get("tick") in spans:
            if spans[ev["tick"]]["restore"] is None:
                spans[ev["tick"]]["restore"] = parse_ts(ev["ts"])
        elif kind == "node-state" and ev.get("state") == "converging":
            if last is not None and last["restore"] is not None and last["restored"] is None:
                last["restored"] = parse_ts(ev["ts"])
        elif kind == "converged" and ev.get("tick") in spans:
            spans[ev["tick"]]["end"] = parse_ts(ev["ts"])
    for span in spans.values():
        if span["end"] is None:
            span["end"] = run_end
        if span["restore"] is None:
            span["restore"] = span["end"]
        if span["restored"] is None:
            span["restored"] = span["restore"]
        span["label"] = fault_label(span)
    return spans


def fault_label(span):
    """The action as the candidates name it: a planned restart that drained
    and a config-flip that reloaded are different faults from their
    undrained and restarting namesakes."""
    notes = [n for n in (span["mode"], "drained" if span["drain"] else "") if n]
    return span["action"] + (f" ({', '.join(notes)})" if notes else "")


def probe_windows(events, run_end):
    """Pair probe-transition events into down windows. A window still open
    at the end of the journal ends at the run end."""
    open_at, windows = {}, []
    for ev in events:
        if ev.get("event") != "probe-transition":
            continue
        ts = parse_ts(ev["ts"])
        if not ev["up"]:
            open_at.setdefault(ev["probe"], ts)
        elif ev["probe"] in open_at:
            windows.append({"probe": ev["probe"], "start": open_at.pop(ev["probe"]), "end": ts})
    for probe, start in open_at.items():
        windows.append({"probe": probe, "start": start, "end": run_end})
    return windows


def loss_windows(events, run_end, spans=None):
    """Pair probe-transition events into down windows, attributed to the
    fault whose inject→converged span they overlap (the report renderer's
    duringFaults logic, reduced to the primary attribution)."""
    faults = list((fault_spans(events, run_end) if spans is None else spans).values())
    windows = probe_windows(events, run_end)
    for w in windows:
        w["ms"] = (w["end"] - w["start"]).total_seconds() * 1000
        hits = [f for f in faults if w["start"] < f["end"] and w["end"] > f["inject"]]
        if hits:
            w["fault"], w["attribution"] = hits[0], "during"
        else:
            before = [f for f in faults if f["inject"] < w["start"]]
            w["fault"], w["attribution"] = (before[-1] if before else None), "after"
        w["action"] = w["fault"]["action"] if w["fault"] else "(none)"
    return windows


def downtime_from_journal(span, windows):
    """down_ms and down_windows for one recovery, as the runner computes
    them: the probe windows overlapping (span.inject, span.end], clipped
    at the inject, summed per probe and counted per probe."""
    down, count = {}, {}
    for w in windows:
        if w["end"] <= span["inject"] or w["start"] >= span["end"]:
            continue
        ms = (min(w["end"], span["end"]) - max(w["start"], span["inject"])).total_seconds() * 1000
        down[w["probe"]] = down.get(w["probe"], 0) + ms
        count[w["probe"]] = count.get(w["probe"], 0) + 1
    return down, count


def phase_slices(w):
    """Cut one attributed loss window at its fault's restore into
    (phase, ms) slices:

    failover — opened and closed while the fault was held
    dark     — still open when the restore began: the time until the
               restore had returned
    tail     — the rest of such a window, after the restore had returned
    restore  — opened after the restore began (for a fault with no hold,
               the reaction to the fault itself)
    after    — opened after the engine declared convergence
    """
    f = w["fault"]
    if w["attribution"] == "after":
        return [("after", w["ms"])]
    if w["start"] >= f["restore"]:
        return [("restore", w["ms"])]
    if w["end"] <= f["restore"]:
        return [("failover", w["ms"])]
    slices = [("dark", (min(w["end"], f["restored"]) - w["start"]).total_seconds() * 1000)]
    if w["end"] > f["restored"]:
        slices.append(("tail", (w["end"] - f["restored"]).total_seconds() * 1000))
    return slices


def by_design(profile, w, phase, repoints):
    """Why the lab, not the agent, owns this slice of loss — or None. The
    tail after a restore stays open: it is the recovery path."""
    if w["probe"] == "pf-vip" and any(w["start"] - REPOINT_LEAD <= r <= w["end"] + REPOINT_LAG for r in repoints):
        return "pf-vip re-pointed by the runner"
    f = w["fault"]
    if f is None or phase not in ("failover", "dark"):
        return None
    if f["action"] == "upstream-bgp-restart":
        return "single upstream router"
    if f["action"] == "double-failover":
        pair = frozenset((f["target"], f["peer"]))
        if w["probe"] in UNBOUND_BY_PAIR.get(pair, ()):
            return "both chassis of the router down"
        vip_pair, vip_probes = VIP_PAIR_BY_PROFILE.get(profile, (None, ()))
        if pair == vip_pair and w["probe"] in vip_probes:
            return "both API VIP gateways down"
    return None


def analyze(roots):
    runs, per_action, per_probe, windows_by_action, slices, faults = [], {}, {}, {}, [], {}
    for path in find_records(roots):
        rec, events = load_record(path)
        if rec is None:
            continue
        start, end = parse_ts(rec["started_at"]), parse_ts(rec["ended_at"])
        label = path.parent.name or str(path)
        run = {
            "label": label,
            "profile": rec["inputs"]["profile"],
            "seed": rec["inputs"]["seed"],
            "result": rec["result"],
            "started_at": rec["started_at"],
            "wall_s": (end - start).total_seconds(),
            "ticks": rec["ticks"],
            "executed": rec["decisions"]["executed"],
            "check_errors": rec.get("checks", {}).get("errors", 0),
            # None: the record predates #239 and counts these in check_errors.
            "check_skipped": rec.get("checks", {}).get("skipped_under_fault"),
            "violations": len(rec.get("violations", [])),
            "sent": sum(p["sent"] for p in rec["probes"].values()),
            "lost": sum(p["lost"] for p in rec["probes"].values()),
            "downtime_derived": 0,
            "downtime_missing": 0,
            "residual_legacy": 0,
        }
        runs.append(run)
        spans = fault_spans(events, end, run["profile"])
        journal_windows = loss_windows(events, end, spans)
        for span in spans.values():
            faults[span["label"]] = faults.get(span["label"], 0) + 1
        repoints = [parse_ts(ev["ts"]) for ev in events
                    if ev.get("event") == "vip-repoint" and ev.get("phase") != "start"]

        for name, p in rec["probes"].items():
            agg = per_probe.setdefault(name, {"sent": 0, "lost": 0, "transitions": 0, "target": p["target"]})
            agg["sent"] += p["sent"]
            agg["lost"] += p["lost"]
            agg["transitions"] += p["transitions"]

        for r in rec.get("recoveries", []):
            down, down_windows = r.get("down_ms"), r.get("down_windows")
            legacy = down is None
            span = spans.get(r["tick"])
            if legacy:
                # A record written before down_ms existed: the journal
                # holds the same windows the runner would have summed.
                if span is not None:
                    down, down_windows = downtime_from_journal(span, journal_windows)
                    run["downtime_derived"] += 1
                else:
                    down, down_windows = {}, {}
                    run["downtime_missing"] += 1
            elif span is not None:
                derived, _ = downtime_from_journal(span, journal_windows)
                for probe in sorted(down.keys() | derived.keys()):
                    a, b = down.get(probe, 0), derived.get(probe, 0)
                    if abs(a - b) > PROBE_INTERVAL_MS:
                        print(f"{path}: tick {r['tick']} down_ms disagrees with the journal: "
                              f"{probe} record {a} ms, journal {b:.0f} ms", file=sys.stderr)

            agg = per_action.setdefault(r["action"], {
                "events": 0, "converged_ms": [], "budget_ms": r["budget_ms"],
                "worst_downtime_ms": [], "downtime_events": 0, "windows": 0,
                "downtime_sum_s": 0.0, "residual_sum_s": 0.0, "residual_events": 0, "worst": None})
            agg["events"] += 1
            agg["converged_ms"].append(r["converged_ms"])
            worst = max(down.values(), default=0)
            agg["worst_downtime_ms"].append(worst)
            if worst > 0:
                agg["downtime_events"] += 1
            agg["windows"] += sum(down_windows.values())
            agg["downtime_sum_s"] += sum(down.values()) / 1000
            if legacy:
                # Before down_ms, from_restore_ms was the restore→last-recovery
                # span, not a window sum; it would overstate the residual.
                run["residual_legacy"] += 1
            else:
                agg["residual_sum_s"] += sum(r.get("from_restore_ms", {}).values()) / 1000
                agg["residual_events"] += 1
            if agg["worst"] is None or worst > agg["worst"]["worst_ms"]:
                agg["worst"] = {"worst_ms": worst, "run": label, "tick": r["tick"],
                                "target": r.get("target", ""), "converged_ms": r["converged_ms"],
                                "probes": {k: v for k, v in down.items() if v > 0}}

        for w in journal_windows:
            key = (w["action"], w["attribution"])
            agg = windows_by_action.setdefault(key, {"count": 0, "total_ms": 0.0, "max_ms": 0.0})
            agg["count"] += 1
            agg["total_ms"] += w["ms"]
            agg["max_ms"] = max(agg["max_ms"], w["ms"])
            f = w["fault"]
            for phase, ms in phase_slices(w):
                slices.append({
                    "run": label, "tick": f["tick"] if f else 0, "target": f["target"] if f else "",
                    "action": f["label"] if f else w["action"], "phase": phase, "probe": w["probe"], "ms": ms,
                    "blip": w["ms"] < BLIP_MS, "slow": bool(f) and f["cadence"] != FAST_CADENCE,
                    "by_design": by_design(run["profile"], w, phase, repoints)})

    return {"runs": runs, "actions": per_action, "probes": per_probe, "windows": windows_by_action,
            "slices": slices, "faults": faults}


def phase_rows(slices, faults):
    """The open loss — every slice the lab does not own by design — per
    action and phase, costliest first. An event's downtime in a phase is
    its worst probe's, so a profile with more probes does not weigh more.
    `faults` counts the injected faults per action, hit or not."""
    cells = {}
    for s in slices:
        if s["by_design"]:
            continue
        cell = cells.setdefault((s["action"], s["phase"]), {"events": {}, "windows": 0, "blips": 0, "probes": {}})
        cell["windows"] += 1
        cell["blips"] += s["blip"]
        cell["probes"][s["probe"]] = cell["probes"].get(s["probe"], 0) + s["ms"]
        event = cell["events"].setdefault((s["run"], s["tick"]),
                                          {"target": s["target"], "slow": s["slow"], "probes": {}})
        event["probes"][s["probe"]] = event["probes"].get(s["probe"], 0) + s["ms"]
    rows = []
    for (action, phase), cell in cells.items():
        worst = {key: max(ev["probes"].values()) for key, ev in cell["events"].items()}
        top = max(worst, key=worst.get)
        slow = [ms for key, ms in worst.items() if cell["events"][key]["slow"]]
        total = faults.get(action, 0)
        rows.append({
            "action": action, "phase": phase, "events": total, "hit": len(worst),
            "windows": cell["windows"], "blips": cell["blips"],
            "downtime_ms": sum(worst.values()),
            "per_event_ms": sum(worst.values()) / total if total else None,
            "median_hit_ms": statistics.median(worst.values()), "max_hit_ms": worst[top],
            "slow_hit": len(slow), "median_slow_hit_ms": statistics.median(slow) if slow else None,
            "probe_down_ms": sum(cell["probes"].values()),
            "probes": dict(sorted(cell["probes"].items(), key=lambda kv: -kv[1])),
            "worst": {"run": top[0], "tick": top[1], "target": cell["events"][top]["target"]}})
    return sorted(rows, key=lambda r: -r["downtime_ms"])


def design_rows(slices):
    cells = {}
    for s in slices:
        if not s["by_design"]:
            continue
        cell = cells.setdefault((s["by_design"], s["action"]), {"windows": 0, "ms": 0.0, "probes": set()})
        cell["windows"] += 1
        cell["ms"] += s["ms"]
        cell["probes"].add(s["probe"])
    return sorted(({"reason": k[0], "action": k[1], "windows": c["windows"], "probe_down_ms": c["ms"],
                    "probes": sorted(c["probes"])} for k, c in cells.items()), key=lambda r: -r["probe_down_ms"])


def fmt_ms(ms):
    return f"{ms / 1000:.1f} s" if ms >= 1000 else f"{ms:.0f} ms"


def render_md(a, top):
    runs, per_action, per_probe, windows_by_action, slices = (
        a["runs"], a["actions"], a["probes"], a["windows"], a["slices"])
    out = []
    total_sent = sum(r["sent"] for r in runs)
    total_lost = sum(r["lost"] for r in runs)
    out.append(f"# Chaos smoothness report — {len(runs)} run records\n")
    out.append(f"Overall probe loss: **{total_lost}/{total_sent}** "
               f"(**{100 * total_lost / max(total_sent, 1):.2f}%**) · "
               f"results: {', '.join(sorted({r['result'] for r in runs}))}\n")
    designed = sum(s["ms"] for s in slices if s["by_design"])
    opened = sum(s["ms"] for s in slices if not s["by_design"])
    out.append(f"Probe-down time in loss windows: **{fmt_ms(designed + opened)}** — "
               f"{fmt_ms(designed)} the lab causes by design, **{fmt_ms(opened)} open**.\n")
    derived = sum(r["downtime_derived"] for r in runs)
    missing = sum(r["downtime_missing"] for r in runs)
    if derived or missing:
        out.append(f"`down_ms` derived from the journal for {derived} recovery events of records "
                   f"written before the field existed; {missing} events had neither and count as "
                   f"no downtime.\n")
    legacy = sum(r["residual_legacy"] for r in runs)
    if legacy:
        out.append(f"`residual` leaves out {legacy} recovery events whose `from_restore_ms` "
                   f"predates `down_ms` and is a restore→last-recovery span.\n")

    out.append("## Runs\n")
    out.append("| record | profile | seed | result | loss | check errors | skipped under fault | violations |")
    out.append("| --- | --- | --- | --- | --- | --- | --- | --- |")
    for r in sorted(runs, key=lambda r: (r["started_at"], r["profile"])):
        skipped = "n/a" if r["check_skipped"] is None else r["check_skipped"]
        out.append(f"| {r['label']} | {r['profile']} | {r['seed']} | {r['result']} "
                   f"| {r['lost']}/{r['sent']} ({100 * r['lost'] / max(r['sent'], 1):.1f}%) "
                   f"| {r['check_errors']} | {skipped} | {r['violations']} |")
    out.append("")

    out.append("## Open loss by action and phase — the optimisation candidates\n")
    out.append("Loss the lab does not cause by design, cut at each fault's restore: `failover` closed "
               "during the hold, `dark` lasted until the restore had returned, `tail` is the rest of a "
               "dark window, `restore` opened after the restore began, `after` opened after convergence. "
               "An event's downtime is its worst probe's. `per event` spreads it over every event of the "
               "action, hit or not. A blip is a window of one lost probe. `slow cadence` is the median of "
               f"the hit events whose target reconciled on another interval than {FAST_CADENCE}.\n")
    out.append("| action | phase | events hit | windows (blips) | downtime | per event | median hit | max hit | slow cadence | probes | worst event |")
    out.append("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for r in phase_rows(slices, a["faults"]):
        per_event = fmt_ms(r["per_event_ms"]) if r["per_event_ms"] is not None else "n/a"
        slow = f"{fmt_ms(r['median_slow_hit_ms'])} ({r['slow_hit']})" if r["slow_hit"] else "—"
        probes = ", ".join(f"{k} {fmt_ms(v)}" for k, v in list(r["probes"].items())[:4])
        if len(r["probes"]) > 4:
            probes += f", +{len(r['probes']) - 4}"
        w = r["worst"]
        out.append(f"| {r['action']} | {r['phase']} | {r['hit']}/{r['events'] or '?'} "
                   f"| {r['windows']} ({r['blips']}) | {fmt_ms(r['downtime_ms'])} | {per_event} "
                   f"| {fmt_ms(r['median_hit_ms'])} | {fmt_ms(r['max_hit_ms'])} | {slow} | {probes} "
                   f"| {w['run']}:{w['tick']} {w['target']} |")
    out.append("")

    rows = design_rows(slices)
    if rows:
        out.append("## Loss the lab causes by design\n")
        out.append("Left out of the candidates above. The rules are `by_design` in analyze.py.\n")
        out.append("| reason | action | windows | probe-down | probes |")
        out.append("| --- | --- | --- | --- | --- |")
        for r in rows:
            out.append(f"| {r['reason']} | {r['action']} | {r['windows']} | {fmt_ms(r['probe_down_ms'])} "
                       f"| {', '.join(r['probes'])} |")
        out.append("")

    out.append("## Downtime by fault action — summed loss windows per event (`down_ms`)\n")
    out.append("Sorted by total probe-down seconds across all runs. `down_ms` sums every probe's "
               "red windows between the inject and the convergence; `residual` is the part of it "
               "after the restore. By-design loss is included.\n")
    out.append("| action | events | with downtime | windows | median worst | max worst | total probe-down | residual | median converged | budget |")
    out.append("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
    for name, a in sorted(per_action.items(), key=lambda kv: -kv[1]["downtime_sum_s"]):
        if a["residual_events"] == 0:
            residual = "n/a"
        elif a["residual_events"] < a["events"]:
            residual = f"{a['residual_sum_s']:.1f} s ({a['residual_events']}/{a['events']})"
        else:
            residual = f"{a['residual_sum_s']:.1f} s"
        out.append(f"| {name} | {a['events']} | {a['downtime_events']} | {a['windows']} "
                   f"| {fmt_ms(statistics.median(a['worst_downtime_ms']))} "
                   f"| {fmt_ms(max(a['worst_downtime_ms']))} "
                   f"| {a['downtime_sum_s']:.1f} s | {residual} "
                   f"| {fmt_ms(statistics.median(a['converged_ms']))} | {fmt_ms(a['budget_ms'])} |")
    out.append("")

    out.append(f"## Worst single events — top {top}\n")
    out.append("| action | run record | tick | target | worst probe downtime | probes hit |")
    out.append("| --- | --- | --- | --- | --- | --- |")
    worsts = [(name, a["worst"]) for name, a in per_action.items() if a["worst"] and a["worst"]["worst_ms"] > 0]
    for name, w in sorted(worsts, key=lambda kv: -kv[1]["worst_ms"])[:top]:
        probes = ", ".join(f"{k} {fmt_ms(v)}" for k, v in sorted(w["probes"].items(), key=lambda kv: -kv[1]))
        out.append(f"| {name} | {w['run']} | {w['tick']} | {w['target']} | {fmt_ms(w['worst_ms'])} | {probes} |")
    out.append("")

    out.append("## Loss windows by fault action (journal)\n")
    out.append("`after` rows are loss that surfaced only after the engine declared convergence — "
               "the smoothness gap the budgets never see.\n")
    out.append("| action | attribution | windows | total down | max down |")
    out.append("| --- | --- | --- | --- | --- |")
    for (action, attribution), a in sorted(windows_by_action.items(), key=lambda kv: -kv[1]["total_ms"]):
        out.append(f"| {action} | {attribution} | {a['count']} | {fmt_ms(a['total_ms'])} | {fmt_ms(a['max_ms'])} |")
    out.append("")

    out.append("## Loss by probe\n")
    out.append("| probe | target | sent | lost | loss | transitions |")
    out.append("| --- | --- | --- | --- | --- | --- |")
    for name, p in sorted(per_probe.items(), key=lambda kv: -kv[1]["lost"]):
        out.append(f"| {name} | {p['target']} | {p['sent']} | {p['lost']} "
                   f"| {100 * p['lost'] / max(p['sent'], 1):.2f}% | {p['transitions']} |")
    out.append("")
    return "\n".join(out)


def render_compare(base_rows, base_runs, new_rows, new_runs):
    """The open loss of two sets of runs side by side. Pooled loss follows
    the seeds; the downtime per event of one action in one phase does not."""
    base = {(r["action"], r["phase"]): r for r in base_rows}
    new = {(r["action"], r["phase"]): r for r in new_rows}

    def per_event(r):
        return r["per_event_ms"] or 0 if r else 0

    def cell(r, key):
        return fmt_ms(r[key]) if r and r[key] is not None else "—"

    out = [f"## Open loss against the baseline — {base_runs} baseline records, {new_runs} new\n"]
    out.append("Baseline → new, by the change in downtime per event. Few events make a noisy row: "
               "read `events hit` before the delta.\n")
    out.append("| action | phase | events hit | per event | median hit | max hit | delta per event |")
    out.append("| --- | --- | --- | --- | --- | --- | --- |")
    for key in sorted(base.keys() | new.keys(), key=lambda k: -abs(per_event(new.get(k)) - per_event(base.get(k)))):
        b, n = base.get(key), new.get(key)
        hits = " → ".join(f"{r['hit']}/{r['events'] or '?'}" if r else "0" for r in (b, n))
        delta = per_event(n) - per_event(b)
        out.append(f"| {key[0]} | {key[1]} | {hits} | {cell(b, 'per_event_ms')} → {cell(n, 'per_event_ms')} "
                   f"| {cell(b, 'median_hit_ms')} → {cell(n, 'median_hit_ms')} "
                   f"| {cell(b, 'max_hit_ms')} → {cell(n, 'max_hit_ms')} "
                   f"| {'+' if delta > 0 else '−' if delta < 0 else ''}{fmt_ms(abs(delta))} |")
    out.append("")
    return "\n".join(out)


def describe(ev):
    kind = ev["event"]
    if kind == "probe-transition":
        return f"{ev['probe']} {'up' if ev['up'] else 'DOWN'}"
    if kind == "decision":
        tail = "executed" if ev.get("executed") else f"skipped ({ev.get('skip_reason', '')})"
        hold = f", hold {fmt_ms(ev['hold_ms'])}" if ev.get("hold_ms") else ""
        return f"{ev.get('action', '')} → {ev.get('target', '')}{hold}, {tail}"
    fields = [str(ev[k]) for k in ("action", "target", "peer", "state", "phase", "flip", "mode", "object",
                                   "kind", "result") if ev.get(k)]
    if ev.get("from") or ev.get("to"):
        fields.append(f"{ev.get('from', '')} → {ev.get('to', '')}")
    for key in ("drain", "rejected"):
        if ev.get(key) is not None:
            fields.append(f"{key}={str(ev[key]).lower()}")
    if ev.get("cr_owner"):
        fields.append(f"cr_owner={ev['cr_owner']}")
    if ev.get("converged_ms"):
        fields.append(f"converged in {fmt_ms(ev['converged_ms'])}")
    if ev.get("detail"):
        fields.append(ev["detail"][:120])
    return " ".join(fields)


def render_timeline(roots, spec, context_s):
    """The journal around one fault, offsets from its inject."""
    name, _, tick = spec.rpartition(":")
    paths = [p for p in find_records(roots) if name in str(p)]
    if len(paths) != 1 or not tick.isdigit():
        print(f"--timeline {spec}: want RECORD:TICK with RECORD matching one record, "
              f"matched {[p.parent.name for p in paths]}", file=sys.stderr)
        return None
    rec, events = load_record(paths[0])
    if rec is None:
        return None
    end = parse_ts(rec["ended_at"])
    spans = fault_spans(events, end, rec["inputs"]["profile"])
    span = spans.get(int(tick))
    if span is None:
        print(f"--timeline {spec}: tick {tick} injected no fault; executed ticks: {sorted(spans)}", file=sys.stderr)
        return None

    def offset(ts):
        return f"{(ts - span['inject']).total_seconds():+.2f} s"

    inputs = rec["inputs"]
    duration = f"{inputs['duration_ms'] // 1000}s"
    out = [f"# {paths[0].parent.name} — tick {tick}: {span['action']} → {span['target']}"
           + (f" + {span['peer']}" if span["peer"] else "") + "\n"]
    out.append(f"Profile `{inputs['profile']}`, seed {inputs['seed']} · restore began {offset(span['restore'])}, "
               f"returned {offset(span['restored'])}, converged {offset(span['end'])}"
               + (f" · drain {'on' if span['drain'] else 'off'}" if span["drain"] is not None else "")
               + (f" · target reconciles every {span['cadence']}" if span["target"].startswith("gateway") else "")
               + "\n")
    earlier = [f"{s['tick']} {s['action']} → {s['target']}" for s in spans.values() if s["tick"] < span["tick"]]
    if earlier:
        out.append(f"Faults before it: {'; '.join(earlier)}\n")
    out.append(f"Replay: `make e2e-chaos CHAOS_FLAGS=\"-seed {inputs['seed']} -profile {inputs['profile']} "
               f"-duration {duration}\"` · `gh workflow run \"E2E Chaos\" -f seed={inputs['seed']} "
               f"-f profile={inputs['profile']} -f duration={duration}`\n")
    out.append("| offset | event | detail |")
    out.append("| --- | --- | --- |")
    lo, hi = span["inject"] - timedelta(seconds=context_s), span["end"] + timedelta(seconds=context_s)
    down_at = {}
    for ev in events:
        ts = parse_ts(ev["ts"])
        detail = describe(ev)
        if ev["event"] == "probe-transition":
            # Tracked from the start of the journal, so a window that
            # opened before the excerpt still reports its length.
            if not ev["up"]:
                down_at[ev["probe"]] = ts
            elif ev["probe"] in down_at:
                detail += f" (down {fmt_ms((ts - down_at.pop(ev['probe'])).total_seconds() * 1000)})"
        if lo <= ts <= hi:
            out.append(f"| {offset(ts)} | {ev['event']} | {detail} |")
    out.append("")
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("dirs", nargs="+", help="artifact directories (each holding summary.json, or parents of such)")
    ap.add_argument("--format", choices=["md", "json"], default="md")
    ap.add_argument("--top", type=int, default=15, help="rows in the worst-events table")
    ap.add_argument("--baseline", nargs="+", metavar="DIR",
                    help="artifact directories of the runs to compare against")
    ap.add_argument("--timeline", metavar="RECORD:TICK", help="print the journal around one fault instead")
    ap.add_argument("--context", type=float, default=10, metavar="SECONDS",
                    help="how far a timeline reaches before the inject and past the convergence")
    args = ap.parse_args()

    if args.timeline:
        timeline = render_timeline(args.dirs, args.timeline, args.context)
        if timeline is None:
            return 1
        print(timeline)
        return 0

    result = analyze(args.dirs)
    if not result["runs"]:
        print("no chaos run records found", file=sys.stderr)
        return 1
    rows = phase_rows(result["slices"], result["faults"])
    base = None
    if args.baseline:
        base = analyze(args.baseline)
        if not base["runs"]:
            print("no chaos run records found under --baseline", file=sys.stderr)
            return 1
        base_rows = phase_rows(base["slices"], base["faults"])
    if args.format == "json":
        for a in result["actions"].values():
            a["median_converged_ms"] = statistics.median(a["converged_ms"])
            a["median_worst_downtime_ms"] = statistics.median(a["worst_downtime_ms"])
            a["max_worst_downtime_ms"] = max(a["worst_downtime_ms"])
            del a["converged_ms"], a["worst_downtime_ms"]
        doc = {
            "runs": result["runs"],
            "actions": result["actions"],
            "probes": result["probes"],
            "loss_windows": [
                {"action": k[0], "attribution": k[1], **v} for k, v in result["windows"].items()],
            "open_loss": rows,
            "by_design": design_rows(result["slices"]),
        }
        if base:
            doc["baseline_open_loss"] = base_rows
        print(json.dumps(doc, indent=2))
    else:
        print(render_md(result, args.top))
        if base:
            print(render_compare(base_rows, len(base["runs"]), rows, len(result["runs"])))
    return 0


if __name__ == "__main__":
    sys.exit(main())
