"""Tests for analyze.py.

    python3 -m unittest discover -s .claude/skills/chaos-analysis
"""

import json
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

import analyze

T0 = datetime(2026, 9, 28, 10, 11, 57, tzinfo=timezone.utc)
RUN_END = analyze.parse_ts("2026-09-28T10:12:57Z")


def ts(seconds):
    return (T0 + timedelta(seconds=seconds)).isoformat().replace("+00:00", "Z")


def fault(event, at):
    return {"event": event, "tick": 1, "action": "gateway-kill", "target": "gateway-1", "ts": ts(at)}


def red(probe, at):
    return {"event": "probe-transition", "probe": probe, "up": False, "ts": ts(at)}


def green(probe, at):
    return {"event": "probe-transition", "probe": probe, "up": True, "ts": ts(at)}


# A failover 2 s after the inject and a 90 ms blink at the restore: two
# windows, 1.29 s in all, although the probe was last green 40.09 s in.
FAILOVER = [fault("inject", 0), red("fip-vm1", 2), green("fip-vm1", 3.2), fault("restore", 39.9),
            red("fip-vm1", 40), green("fip-vm1", 40.09), fault("converged", 45)]


def downtime(events):
    return analyze.downtime_from_journal(analyze.fault_spans(events, RUN_END)[1],
                                         analyze.probe_windows(events, RUN_END))


class DowntimeFromJournal(unittest.TestCase):
    def assertDowntime(self, events, want_ms, want_windows):
        down, windows = downtime(events)
        self.assertEqual(down.keys(), want_ms.keys())
        for probe, ms in want_ms.items():
            self.assertAlmostEqual(down[probe], ms, places=6)
        self.assertEqual(windows, want_windows)

    def test_sums_the_windows_between_inject_and_convergence(self):
        self.assertDowntime(FAILOVER, {"fip-vm1": 1290}, {"fip-vm1": 2})

    def test_clips_windows_at_the_inject_and_the_convergence(self):
        events = [red("fip-vm1", -1), fault("inject", 0), green("fip-vm1", 0.5),
                  red("fip-vm1", 44), fault("converged", 45), green("fip-vm1", 50)]
        self.assertDowntime(events, {"fip-vm1": 1500}, {"fip-vm1": 2})

    def test_skips_windows_that_only_touch_the_span(self):
        events = [red("fip-vm1", -2), green("fip-vm1", 0), fault("inject", 0),
                  fault("converged", 45), red("fip-vm1", 45), green("fip-vm1", 46)]
        self.assertDowntime(events, {}, {})

    def test_an_open_fault_and_an_open_window_end_at_the_run_end(self):
        self.assertDowntime([fault("inject", 50), red("pf-vip", 55)], {"pf-vip": 5000}, {"pf-vip": 1})


def record(recovery):
    return {
        "schema": analyze.RECORD_SCHEMA,
        "started_at": ts(-10), "ended_at": ts(60),
        "inputs": {"profile": "everything-on", "seed": 7},
        "result": "pass", "ticks": 1, "decisions": {"executed": 1},
        "probes": {"fip-vm1": {"target": "192.0.2.10", "sent": 70, "lost": 2, "transitions": 4}},
        "recoveries": [{"tick": 1, "action": "gateway-kill", "target": "gateway-1",
                        "budget_ms": 90_000, "converged_ms": 5_100, **recovery}],
    }


# Before down_ms, from_restore_ms spanned the restore to the last recovery.
OLD = record({"from_restore_ms": {"fip-vm1": 190}})
NEW = record({"down_ms": {"fip-vm1": 1290}, "down_windows": {"fip-vm1": 2},
              "from_restore_ms": {"fip-vm1": 90}})


def analyze_records(records):
    with tempfile.TemporaryDirectory() as tmp:
        for name, rec in records.items():
            run = Path(tmp) / name
            run.mkdir()
            (run / "summary.json").write_text(json.dumps(rec))
            (run / "journal.jsonl").write_text("".join(json.dumps(ev) + "\n" for ev in FAILOVER))
        return analyze.analyze([tmp])


class Analyze(unittest.TestCase):
    def test_residual_leaves_out_a_from_restore_ms_that_predates_down_ms(self):
        runs, per_action, per_probe, windows_by_action = analyze_records({"old": OLD, "new": NEW})

        agg = per_action["gateway-kill"]
        self.assertAlmostEqual(agg["downtime_sum_s"], 2.58, places=6)
        self.assertAlmostEqual(agg["residual_sum_s"], 0.09, places=6)
        self.assertEqual(agg["residual_events"], 1)
        self.assertEqual(sum(r["residual_legacy"] for r in runs), 1)
        report = analyze.render_md(runs, per_action, per_probe, windows_by_action, 15)
        self.assertIn("`residual` leaves out 1 recovery events", report)
        self.assertIn("| 2.6 s | 0.1 s (1/2) |", report)

    def test_residual_is_not_available_when_no_event_measured_it(self):
        runs, per_action, per_probe, windows_by_action = analyze_records({"old": OLD})

        self.assertEqual(per_action["gateway-kill"]["residual_events"], 0)
        self.assertIn("| 1.3 s | n/a |",
                      analyze.render_md(runs, per_action, per_probe, windows_by_action, 15))


if __name__ == "__main__":
    unittest.main()
