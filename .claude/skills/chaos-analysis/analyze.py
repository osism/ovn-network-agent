#!/usr/bin/env python3
"""Aggregate chaos-run records across runs into a smoothness report.

Reads every summary.json (+ journal.jsonl next to it, when present) under
the given directories — the layout `gh run download` produces — and
answers the question the per-run report cannot: which fault actions cost
the most packet loss and downtime *across* runs, and where the recovery
budget headroom is thinnest.

Usage:
    analyze.py DIR [DIR ...] [--format md|json] [--top N]

Each DIR may be a single artifact directory (holding summary.json) or a
parent holding one subdirectory per profile/run. Output goes to stdout.
"""

import argparse
import json
import statistics
import sys
from datetime import datetime
from pathlib import Path

RECORD_SCHEMA = "chaos-run-record/v1"
# Mirrors probeInterval in test/e2e/chaos/probe.go: a record and its
# journal may disagree by up to one probe period.
PROBE_INTERVAL_MS = 1000


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


def fault_spans(events, run_end):
    """The inject→converged span of every injected fault, keyed by tick in
    journal order. A fault that never converged spans to the run end."""
    spans = {}
    for ev in events:
        if ev.get("event") == "inject":
            spans[ev["tick"]] = {"action": ev["action"], "target": ev.get("target", ""),
                                 "inject": parse_ts(ev["ts"]), "end": None}
        elif ev.get("event") == "converged" and ev.get("tick") in spans:
            spans[ev["tick"]]["end"] = parse_ts(ev["ts"])
    for span in spans.values():
        if span["end"] is None:
            span["end"] = run_end
    return spans


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


def loss_windows(events, run_end):
    """Pair probe-transition events into down windows, attributed to the
    fault whose inject→converged span they overlap (the report renderer's
    duringFaults logic, reduced to the primary attribution)."""
    faults = list(fault_spans(events, run_end).values())
    windows = probe_windows(events, run_end)
    for w in windows:
        w["ms"] = (w["end"] - w["start"]).total_seconds() * 1000
        hits = [f for f in faults if w["start"] < f["end"] and w["end"] > f["inject"]]
        if hits:
            w["action"], w["attribution"] = hits[0]["action"], "during"
        else:
            before = [f for f in faults if f["inject"] < w["start"]]
            w["action"] = before[-1]["action"] if before else "(none)"
            w["attribution"] = "after"
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


def analyze(roots):
    runs, per_action, per_probe, windows_by_action = [], {}, {}, {}
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
            "violations": len(rec.get("violations", [])),
            "sent": sum(p["sent"] for p in rec["probes"].values()),
            "lost": sum(p["lost"] for p in rec["probes"].values()),
            "downtime_derived": 0,
            "downtime_missing": 0,
            "residual_legacy": 0,
        }
        runs.append(run)
        spans = fault_spans(events, end)
        journal_windows = loss_windows(events, end)

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

    return runs, per_action, per_probe, windows_by_action


def fmt_ms(ms):
    return f"{ms / 1000:.1f} s" if ms >= 1000 else f"{ms:.0f} ms"


def render_md(runs, per_action, per_probe, windows_by_action, top):
    out = []
    total_sent = sum(r["sent"] for r in runs)
    total_lost = sum(r["lost"] for r in runs)
    out.append(f"# Chaos smoothness report — {len(runs)} run records\n")
    out.append(f"Overall probe loss: **{total_lost}/{total_sent}** "
               f"(**{100 * total_lost / max(total_sent, 1):.2f}%**) · "
               f"results: {', '.join(sorted({r['result'] for r in runs}))}\n")
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
    out.append("| record | profile | seed | result | loss | check errors | violations |")
    out.append("| --- | --- | --- | --- | --- | --- | --- |")
    for r in sorted(runs, key=lambda r: (r["started_at"], r["profile"])):
        out.append(f"| {r['label']} | {r['profile']} | {r['seed']} | {r['result']} "
                   f"| {r['lost']}/{r['sent']} ({100 * r['lost'] / max(r['sent'], 1):.1f}%) "
                   f"| {r['check_errors']} | {r['violations']} |")
    out.append("")

    out.append("## Downtime by fault action — summed loss windows per event (`down_ms`)\n")
    out.append("Sorted by total probe-down seconds across all runs. `down_ms` sums every probe's "
               "red windows between the inject and the convergence; `residual` is the part of it "
               "after the restore.\n")
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


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("dirs", nargs="+", help="artifact directories (each holding summary.json, or parents of such)")
    ap.add_argument("--format", choices=["md", "json"], default="md")
    ap.add_argument("--top", type=int, default=15, help="rows in the worst-events table")
    args = ap.parse_args()

    runs, per_action, per_probe, windows_by_action = analyze(args.dirs)
    if not runs:
        print("no chaos run records found", file=sys.stderr)
        return 1
    if args.format == "json":
        for a in per_action.values():
            a["median_converged_ms"] = statistics.median(a["converged_ms"])
            a["median_worst_downtime_ms"] = statistics.median(a["worst_downtime_ms"])
            a["max_worst_downtime_ms"] = max(a["worst_downtime_ms"])
            del a["converged_ms"], a["worst_downtime_ms"]
        print(json.dumps({
            "runs": runs,
            "actions": per_action,
            "probes": per_probe,
            "loss_windows": [
                {"action": k[0], "attribution": k[1], **v} for k, v in windows_by_action.items()],
        }, indent=2))
    else:
        print(render_md(runs, per_action, per_probe, windows_by_action, args.top))
    return 0


if __name__ == "__main__":
    sys.exit(main())
