"""Tests for analyze.py.

    python3 -m unittest discover -s .claude/skills/chaos-analysis
"""

import contextlib
import io
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


def fault(event, at, **fields):
    return {"event": event, "tick": 1, "action": "gateway-kill", "target": "gateway-1", "ts": ts(at), **fields}


def red(probe, at):
    return {"event": "probe-transition", "probe": probe, "up": False, "ts": ts(at)}


def green(probe, at):
    return {"event": "probe-transition", "probe": probe, "up": True, "ts": ts(at)}


def converging(at, target="gateway-1"):
    return {"event": "node-state", "target": target, "state": "converging", "ts": ts(at)}


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


def slices(events, profile="everything-on", repoints=()):
    """(probe, phase, ms rounded, by_design) of every slice, in window order."""
    spans = analyze.fault_spans(events, RUN_END, profile)
    out = []
    for w in analyze.loss_windows(events, RUN_END, spans):
        for phase, ms in analyze.phase_slices(w):
            out.append((w["probe"], phase, round(ms), analyze.by_design(profile, w, phase, repoints)))
    return out


class PhaseSlices(unittest.TestCase):
    def test_a_failover_and_a_blink_at_the_restore_are_two_phases(self):
        self.assertEqual(slices(FAILOVER), [("fip-vm1", "failover", 1200, None), ("fip-vm1", "restore", 90, None)])

    def test_a_window_open_at_the_restore_is_cut_where_the_restore_returned(self):
        # The restore began at 30 s and took 4 s; the probe came back 2 s later.
        events = [fault("inject", 0), red("fip-vm1", 2), fault("restore", 30), converging(34),
                  green("fip-vm1", 36), fault("converged", 40)]
        self.assertEqual(slices(events), [("fip-vm1", "dark", 32000, None), ("fip-vm1", "tail", 2000, None)])

    def test_a_dark_window_that_closes_while_the_restore_runs_has_no_tail(self):
        events = [fault("inject", 0), red("fip-vm1", 2), fault("restore", 30), green("fip-vm1", 32),
                  converging(34), fault("converged", 40)]
        self.assertEqual(slices(events), [("fip-vm1", "dark", 30000, None)])

    def test_a_window_that_opens_after_the_convergence_is_after(self):
        events = [fault("inject", 0), fault("restore", 0.2), fault("converged", 0.7),
                  red("cross-fip", 1.9), green("cross-fip", 15.2)]
        self.assertEqual(slices(events), [("cross-fip", "after", 13300, None)])


def double_failover(target, peer, probe):
    return [fault("inject", 0, action="double-failover", target=target, peer=peer), red(probe, 2),
            fault("restore", 30, action="double-failover", target=target), converging(34, target),
            green(probe, 36), fault("converged", 40, action="double-failover", target=target)]


class ByDesign(unittest.TestCase):
    def test_an_upstream_restart_owns_the_hold_but_not_the_tail(self):
        events = [fault("inject", 0, action="upstream-bgp-restart", target="upstream"), red("fip-vm1", 1),
                  fault("restore", 10, action="upstream-bgp-restart", target="upstream"),
                  converging(10.2, "upstream"), green("fip-vm1", 13),
                  fault("converged", 15, action="upstream-bgp-restart", target="upstream")]
        self.assertEqual(slices(events), [("fip-vm1", "dark", 9200, "single upstream router"),
                                          ("fip-vm1", "tail", 2800, None)])

    def test_a_double_failover_owns_the_router_it_leaves_without_a_chassis(self):
        self.assertEqual(slices(double_failover("gateway-2", "gateway-3", "cross-fip"))[0][3],
                         "both chassis of the router down")
        self.assertEqual(slices(double_failover("gateway-2", "gateway-1", "fip-vlan101"))[0][3],
                         "both chassis of the router down")

    def test_a_double_failover_does_not_own_a_router_with_a_survivor(self):
        self.assertIsNone(slices(double_failover("gateway-3", "gateway-1", "cross-fip"))[0][3])
        self.assertIsNone(slices(double_failover("gateway-1", "gateway-2", "fip-vm1"))[0][3])

    def test_the_api_vip_pair_is_by_design_only_in_its_profile(self):
        events = double_failover("gateway-1", "gateway-2", "api-vip")
        self.assertEqual(slices(events, profile="heterogeneous")[0][3], "both API VIP gateways down")
        self.assertIsNone(slices(events, profile="flat-dnat")[0][3])

    def test_pf_vip_is_the_runners_when_a_re_point_ends_the_window(self):
        events = [fault("inject", 0), red("pf-vip", 4), green("pf-vip", 9.3), fault("converged", 14)]
        self.assertEqual(slices(events, repoints=[analyze.parse_ts(ts(9))])[0][3],
                         "pf-vip re-pointed by the runner")
        self.assertIsNone(slices(events, repoints=[analyze.parse_ts(ts(30))])[0][3])
        self.assertIsNone(slices(events)[0][3])


class FaultSpans(unittest.TestCase):
    def flip(self, at, to, **fields):
        return {"event": "config-flip", "target": "gateway-1", "flip": "cadence-toggle",
                "from": "5s", "to": to, "rejected": False, "mode": "reload", "ts": ts(at), **fields}

    def test_a_cadence_toggle_changes_the_cadence_of_the_faults_after_it(self):
        events = [fault("inject", 0, action="config-flip"), self.flip(1, "15s"),
                  fault("converged", 2, action="config-flip"),
                  fault("inject", 10, tick=2), fault("converged", 12, tick=2),
                  fault("inject", 20, tick=3, target="gateway-2"), fault("converged", 22, tick=3)]
        spans = analyze.fault_spans(events, RUN_END)
        self.assertEqual([spans[t]["cadence"] for t in (1, 2, 3)], ["5s", "15s", "5s"])
        self.assertEqual(spans[1]["label"], "config-flip (reload)")

    def test_a_rejected_toggle_changes_nothing(self):
        events = [fault("inject", 0, action="config-flip"), self.flip(1, "15s", rejected=True),
                  fault("converged", 2, action="config-flip"), fault("inject", 10, tick=2)]
        spans = analyze.fault_spans(events, RUN_END)
        self.assertEqual(spans[2]["cadence"], "5s")
        self.assertEqual(spans[1]["label"], "config-flip (rejected)")

    def test_a_profile_starts_a_gateway_on_its_own_cadence(self):
        events = [fault("inject", 0, target="gateway-3")]
        self.assertEqual(analyze.fault_spans(events, RUN_END, "heterogeneous")[1]["cadence"], "15s")
        self.assertEqual(analyze.fault_spans(events, RUN_END, "everything-on")[1]["cadence"], "5s")

    def test_a_drained_restart_is_labelled_apart(self):
        events = [fault("inject", 0, action="agent-terminate", drain=True),
                  fault("inject", 50, tick=2, action="agent-terminate", drain=False)]
        spans = analyze.fault_spans(events, RUN_END)
        self.assertEqual([spans[1]["label"], spans[2]["label"]], ["agent-terminate (drained)", "agent-terminate"])


def record(recovery, profile="everything-on"):
    return {
        "schema": analyze.RECORD_SCHEMA,
        "started_at": ts(-10), "ended_at": ts(60),
        "inputs": {"profile": profile, "seed": 7, "duration_ms": 600_000},
        "result": "pass", "ticks": 1, "decisions": {"executed": 1},
        "probes": {"fip-vm1": {"target": "192.0.2.10", "sent": 70, "lost": 2, "transitions": 4}},
        "recoveries": [{"tick": 1, "action": "gateway-kill", "target": "gateway-1",
                        "budget_ms": 90_000, "converged_ms": 5_100, **recovery}],
    }


# Before down_ms, from_restore_ms spanned the restore to the last recovery.
OLD = record({"from_restore_ms": {"fip-vm1": 190}})
NEW = record({"down_ms": {"fip-vm1": 1290}, "down_windows": {"fip-vm1": 2},
              "from_restore_ms": {"fip-vm1": 90}})
# A record that leaves its downtime to whatever journal lies next to it.
BARE = record({})
# The same fault with no loss at all.
QUIET = [fault("inject", 0), fault("restore", 39.9), fault("converged", 45)]


def write_records(tmp, records, journals=None):
    for name, rec in records.items():
        run = Path(tmp) / name
        run.mkdir()
        (run / "summary.json").write_text(json.dumps(rec))
        events = (journals or {}).get(name, FAILOVER)
        (run / "journal.jsonl").write_text("".join(json.dumps(ev) + "\n" for ev in events))


def analyze_records(records, journals=None):
    with tempfile.TemporaryDirectory() as tmp:
        write_records(tmp, records, journals)
        return analyze.analyze([tmp])


class Analyze(unittest.TestCase):
    def test_residual_leaves_out_a_from_restore_ms_that_predates_down_ms(self):
        result = analyze_records({"old": OLD, "new": NEW})

        agg = result["actions"]["gateway-kill"]
        self.assertAlmostEqual(agg["downtime_sum_s"], 2.58, places=6)
        self.assertAlmostEqual(agg["residual_sum_s"], 0.09, places=6)
        self.assertEqual(agg["residual_events"], 1)
        self.assertEqual(sum(r["residual_legacy"] for r in result["runs"]), 1)
        report = analyze.render_md(result, 15)
        self.assertIn("`residual` leaves out 1 recovery events", report)
        self.assertIn("| 2.6 s | 0.1 s (1/2) |", report)

    def test_residual_is_not_available_when_no_event_measured_it(self):
        result = analyze_records({"old": OLD})

        self.assertEqual(result["actions"]["gateway-kill"]["residual_events"], 0)
        self.assertIn("| 1.3 s | n/a |", analyze.render_md(result, 15))

    def test_runs_show_the_sweeps_skipped_under_a_held_fault_apart_from_the_errors(self):
        # Before #239 a record counted those sweeps in checks.errors and had
        # no skipped_under_fault, so its count is not available rather than 0.
        old = {**NEW, "checks": {"sweeps": 60, "dual_claim_evaluated": 55, "errors": 5}}
        new = {**NEW, "checks": {"sweeps": 60, "dual_claim_evaluated": 55, "errors": 0, "skipped_under_fault": 5}}
        report = analyze.render_md(analyze_records({"old": old, "new": new}), 15)

        self.assertIn("| check errors | skipped under fault | violations |", report)
        self.assertIn("| old | everything-on | 7 | pass | 2/70 (2.9%) | 5 | n/a | 0 |", report)
        self.assertIn("| new | everything-on | 7 | pass | 2/70 (2.9%) | 0 | 5 | 0 |", report)


class PhaseRows(unittest.TestCase):
    def test_spreads_the_downtime_over_every_fault_of_the_action(self):
        result = analyze_records({"hit": NEW, "quiet": BARE}, {"quiet": QUIET})

        rows = {r["phase"]: r for r in analyze.phase_rows(result["slices"], result["faults"])}
        self.assertEqual(rows.keys(), {"failover", "restore"})
        failover = rows["failover"]
        self.assertEqual((failover["hit"], failover["events"], failover["windows"], failover["blips"]), (1, 2, 1, 0))
        self.assertAlmostEqual(failover["downtime_ms"], 1200, places=6)
        self.assertAlmostEqual(failover["per_event_ms"], 600, places=6)
        self.assertEqual(failover["worst"], {"run": "hit", "tick": 1, "target": "gateway-1"})
        self.assertEqual(rows["restore"]["blips"], 1)

    def test_an_events_downtime_is_its_worst_probes(self):
        events = [fault("inject", 0), red("fip-vm1", 2), red("fip-vm2", 2), green("fip-vm2", 3),
                  green("fip-vm1", 5), fault("restore", 39.9), fault("converged", 45)]
        result = analyze_records({"run": BARE}, {"run": events})

        (row,) = analyze.phase_rows(result["slices"], result["faults"])
        self.assertAlmostEqual(row["downtime_ms"], 3000, places=6)
        self.assertAlmostEqual(row["probe_down_ms"], 4000, places=6)

    def test_by_design_loss_is_no_candidate(self):
        events = double_failover("gateway-2", "gateway-3", "cross-fip")
        result = analyze_records({"run": BARE}, {"run": events})

        self.assertEqual([(r["action"], r["phase"]) for r in analyze.phase_rows(result["slices"], result["faults"])],
                         [("double-failover", "tail")])
        (designed,) = analyze.design_rows(result["slices"])
        self.assertEqual((designed["reason"], designed["probes"]), ("both chassis of the router down", ["cross-fip"]))
        self.assertAlmostEqual(designed["probe_down_ms"], 32000, places=6)
        self.assertIn("32.0 s the lab causes by design, **2.0 s open**", analyze.render_md(result, 15))

    def test_reports_the_hit_events_on_a_slow_cadence_apart(self):
        slow = [fault("inject", 0, target="gateway-3"), red("fip-vm1", 2), green("fip-vm1", 16),
                fault("restore", 39.9), fault("converged", 45)]
        result = analyze_records({"fast": NEW, "slow": record({}, "heterogeneous")}, {"slow": slow})

        (row,) = [r for r in analyze.phase_rows(result["slices"], result["faults"]) if r["phase"] == "failover"]
        self.assertEqual(row["slow_hit"], 1)
        self.assertAlmostEqual(row["median_slow_hit_ms"], 14000, places=6)
        self.assertIn("| 14.0 s (1) |", analyze.render_md(result, 15))


class Compare(unittest.TestCase):
    def test_lists_the_change_in_downtime_per_event(self):
        base = analyze_records({"a": NEW})
        new = analyze_records({"a": BARE}, {"a": QUIET})

        report = analyze.render_compare(analyze.phase_rows(base["slices"], base["faults"]), 1,
                                        analyze.phase_rows(new["slices"], new["faults"]), 1)
        self.assertIn("| gateway-kill | failover | 1/1 → 0 | 1.2 s → — | 1.2 s → — | 1.2 s → — | −1.2 s |", report)


class Timeline(unittest.TestCase):
    def render(self, spec):
        with tempfile.TemporaryDirectory() as tmp:
            write_records(tmp, {"run-a": NEW, "run-b": NEW})
            return analyze.render_timeline([tmp], spec, 10)

    def test_prints_the_journal_around_the_fault_with_offsets_from_the_inject(self):
        timeline = self.render("run-a:1")

        self.assertIn("# run-a — tick 1: gateway-kill → gateway-1", timeline)
        self.assertIn("-seed 7 -profile everything-on -duration 600s", timeline)
        self.assertIn("| +2.00 s | probe-transition | fip-vm1 DOWN |", timeline)
        self.assertIn("| +3.20 s | probe-transition | fip-vm1 up (down 1.2 s) |", timeline)
        self.assertIn("| +45.00 s | converged | gateway-kill gateway-1 |", timeline)

    def test_wants_one_record_and_an_injected_tick(self):
        with contextlib.redirect_stderr(io.StringIO()) as err:
            self.assertIsNone(self.render("run:1"))
            self.assertIsNone(self.render("run-a:2"))
        self.assertIn("matched ['run-a', 'run-b']", err.getvalue())
        self.assertIn("tick 2 injected no fault", err.getvalue())


if __name__ == "__main__":
    unittest.main()
