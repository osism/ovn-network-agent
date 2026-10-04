package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reportRecord is a small but fully-populated run record: every section
// the renderer knows appears in the output exactly once, so the tests
// can assert on presence and order without a golden file that breaks on
// every wording tweak.
func reportRecord(t *testing.T) *runRecord {
	t.Helper()
	rec := &runRecord{
		Inputs: runInputs{
			Seed: 7, Profile: "pf-only",
			DurationMS:      (3 * time.Minute).Milliseconds(),
			TickMinMS:       (10 * time.Second).Milliseconds(),
			TickMaxMS:       (30 * time.Second).Milliseconds(),
			SettleEveryMS:   (3 * time.Minute).Milliseconds(),
			SettleTimeoutMS: (2 * time.Minute).Milliseconds(),
			Weights:         map[string]int{"frr-restart": 2},
			Lab:             "ovn-e2e",
		},
		StartedAt:     "2026-07-16T19:00:00Z",
		Ticks:         3,
		Decisions:     decisionCounts{Total: 3, Executed: 2, Skipped: 1},
		Checks:        checkCounts{Sweeps: 12, DualClaim: 12},
		ActionsByName: map[string]int{"frr-restart": 1, "mgmt-delay": 1},
		Probes: map[string]probeSummary{
			"pf-vip": {Target: "http://192.0.2.50:80/", Sent: 180, Lost: 0,
				Buckets: []lossBucket{{OffsetMS: 0, Sent: 180, Lost: 0}}},
			"fip-vm1": {Target: "192.0.2.10", Sent: 180, Lost: 4, Transitions: 2,
				Buckets: []lossBucket{{OffsetMS: 60_000, Sent: 10, Lost: 4}}},
		},
		Recoveries: []recoveryRecord{
			{Tick: 1, Action: "mgmt-delay", Target: "gateway-1", BudgetMS: 90_000, ConvergedMS: 150,
				DownMS:        map[string]int64{"fip-vm1": 0, "pf-vip": 0},
				DownWindows:   map[string]int{"fip-vm1": 0, "pf-vip": 0},
				FromRestoreMS: map[string]int64{"fip-vm1": 0, "pf-vip": 0}},
			{Tick: 2, Action: "frr-restart", Target: "gateway-2", BudgetMS: 120_000, ConvergedMS: 5_200,
				DownMS:        map[string]int64{"fip-vm1": 5_100, "pf-vip": 0},
				DownWindows:   map[string]int{"fip-vm1": 2, "pf-vip": 0},
				FromRestoreMS: map[string]int64{"fip-vm1": 3_800, "pf-vip": 0}},
		},
		Settles: []settleRecord{{Tick: 2, ConvergedMS: 1_600, Passed: true}},
	}
	rec.finalize(time.Date(2026, 7, 16, 19, 3, 30, 0, time.UTC))
	return rec
}

// renderToString drives the renderer the way runReport does and fails
// the test on a write error a bytes.Buffer cannot actually produce.
func renderToString(t *testing.T, rec *runRecord, events []event) string {
	t.Helper()
	var buf bytes.Buffer
	w := &mdWriter{w: &buf}
	renderReport(w, rec, events, "")
	if w.err != nil {
		t.Fatalf("render: %v", w.err)
	}
	return buf.String()
}

func TestRenderReportCoversTheRecord(t *testing.T) {
	t.Parallel()

	out := renderToString(t, reportRecord(t), nil)

	for _, want := range []string{
		"✅ pass", "profile `pf-only`, seed 7",
		"3 ticks — 2 executed, 1 skipped",
		"12 baseline sweeps",
		"wall clock 3m30s",
		"`frr-restart` ×1, `mgmt-delay` ×1",
		`CHAOS_FLAGS="-seed 7 -profile pf-only -duration 3m0s -tick-min 10s -tick-max 30s -settle-every 3m0s -settle-timeout 2m0s"`,
		"| 2 | frr-restart | gateway-2 | 5.2 s | 2m0s | fip-vm1 5.1 s | fip-vm1 3.8 s |",
		"| 1 | mgmt-delay | gateway-1 | 150 ms | 1m30s | none | none |",
		"| fip-vm1 | 192.0.2.10 | 180 | 4 | 2.2% | 2 |",
		"| 2 | 1.6 s | pass | 0 |",
		`"weights"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("report is missing %q:\n%s", want, out)
		}
	}
	// A passing run renders no violation section at all — an empty table
	// would read like a check that ran and found nothing to say.
	if strings.Contains(out, "### Violations") {
		t.Fatalf("a passing run rendered a violations section:\n%s", out)
	}
	// Slowest first: the 5.2 s recovery outranks the 150 ms one.
	if strings.Index(out, "frr-restart | gateway-2") > strings.Index(out, "mgmt-delay | gateway-1") {
		t.Fatalf("recoveries are not sorted slowest-first:\n%s", out)
	}
}

// A record written before down_ms existed unmarshals with nil maps: its
// probe loss renders as a dash, since nothing was measured, and the loss
// after the restore still renders.
func TestRenderRecoveriesDashesAnOldRecordsLoss(t *testing.T) {
	t.Parallel()
	rec := reportRecord(t)
	for i := range rec.Recoveries {
		rec.Recoveries[i].DownMS = nil
		rec.Recoveries[i].DownWindows = nil
	}
	dir := writeRunDir(t, rec, nil)
	var buf bytes.Buffer

	if err := runReport(dir, &buf, &fakeCommander{}); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	for _, want := range []string{
		"| 2 | frr-restart | gateway-2 | 5.2 s | 2m0s | — | fip-vm1 3.8 s |",
		"| 1 | mgmt-delay | gateway-1 | 150 ms | 1m30s | — | none |",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("report is missing %q:\n%s", want, buf.String())
		}
	}
}

// The header keeps the sweeps the run could not answer apart from the
// ones a held fault made unanswerable, so a non-zero error count needs no
// trip to the journal to mean something.
func TestRenderReportSeparatesSweepErrorsFromSkips(t *testing.T) {
	t.Parallel()
	rec := reportRecord(t)
	rec.Checks = checkCounts{Sweeps: 12, DualClaim: 9, Errors: 1, SkippedUnderFault: 2}

	out := renderToString(t, rec, nil)

	if want := "12 baseline sweeps (9 dual-claim, 1 errors, 2 skipped under fault)"; !strings.Contains(out, want) {
		t.Fatalf("report is missing %q:\n%s", want, out)
	}
}

// A record written before skipped_under_fault existed has no such key. It
// still loads, and renders the count as zero.
func TestReportReadsARecordWrittenBeforeSkippedSweepsWereCounted(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(reportRecord(t))
	if err != nil {
		t.Fatalf("marshal the record: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("unmarshal the record: %v", err)
	}
	checks, ok := raw["checks"].(map[string]any)
	if !ok {
		t.Fatalf("the record has no checks object: %s", encoded)
	}
	delete(checks, "skipped_under_fault")
	old, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal the old record: %v", err)
	}
	if strings.Contains(string(old), "skipped_under_fault") {
		t.Fatalf("the old record still carries skipped_under_fault: %s", old)
	}
	path := filepath.Join(t.TempDir(), summaryFile)
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatalf("write the old record: %v", err)
	}

	rec, events, err := loadRecord(path)
	if err != nil {
		t.Fatalf("loadRecord: %v", err)
	}

	out := renderToString(t, rec, events)
	if want := "(12 dual-claim, 0 errors, 0 skipped under fault)"; !strings.Contains(out, want) {
		t.Fatalf("report is missing %q:\n%s", want, out)
	}
}

// A run that failed on its own tooling renders its verdict verbatim, so
// the headline alone tells a harness defect from an agent regression.
func TestRenderReportNamesTheHarnessFault(t *testing.T) {
	t.Parallel()
	rec := reportRecord(t)
	rec.Violations = []violationRecord{{
		Kind: violationActionFailed, Tick: 3, Action: "lb-vip-churn", Target: centralNode,
		Detail: "remove the churn VIP entry: exit status 1",
	}}
	rec.finalize(time.Date(2026, 7, 16, 19, 3, 30, 0, time.UTC))

	out := renderToString(t, rec, nil)

	if !strings.Contains(out, "## Chaos run — ❌ harness-fault") {
		t.Fatalf("the harness fault did not reach the headline:\n%s", out)
	}
}

func TestRenderReportNamesTheViolations(t *testing.T) {
	t.Parallel()
	rec := reportRecord(t)
	rec.Violations = []violationRecord{{
		Kind: violationRecoveryTimeout, Tick: 2, Action: "frr-restart", Target: "gateway-2",
		Detail: "pipes | and\nnewlines", JournalOffset: 17,
	}}
	rec.finalize(time.Date(2026, 7, 16, 19, 3, 30, 0, time.UTC))

	out := renderToString(t, rec, nil)

	if !strings.Contains(out, "❌ fail") {
		t.Fatalf("a failed run did not render its verdict:\n%s", out)
	}
	// The detail lands in one table cell: the pipe is escaped and the
	// newline flattened, or the row shears apart in the rendered table.
	if !strings.Contains(out, `pipes \| and newlines`) {
		t.Fatalf("the violation detail broke the table row:\n%s", out)
	}
	if !strings.Contains(out, "| 17 |") {
		t.Fatalf("the journal offset a triager jumps by is missing:\n%s", out)
	}
}

// journalFor renders events as the JSONL the runner writes, so the tests
// exercise the same decode path a real journal takes.
func journalFor(t *testing.T, events []event) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

func TestRenderReportCorrelatesLossWithFaults(t *testing.T) {
	t.Parallel()
	events := []event{
		{TS: "2026-07-16T19:00:30Z", Event: evInject, Tick: 1, Action: "frr-restart", Target: "gateway-2"},
		// This window sits inside tick 1's inject→converged span.
		{TS: "2026-07-16T19:00:32Z", Event: evProbeTransition, Probe: "fip-vm1", Up: boolPtr(false)},
		{TS: "2026-07-16T19:00:36Z", Event: evProbeTransition, Probe: "fip-vm1", Up: boolPtr(true)},
		{TS: "2026-07-16T19:00:40Z", Event: evConverged, Tick: 1, Action: "frr-restart", Target: "gateway-2"},
		// This one starts after the convergence: attributed as "after".
		{TS: "2026-07-16T19:00:50Z", Event: evProbeTransition, Probe: "pf-vip", Up: boolPtr(false)},
		{TS: "2026-07-16T19:00:51Z", Event: evProbeTransition, Probe: "pf-vip", Up: boolPtr(true)},
	}

	out := renderToString(t, reportRecord(t), events)

	if !strings.Contains(out, "| t+00:32 | fip-vm1 | 4.0 s | tick 1 frr-restart → gateway-2 |") {
		t.Fatalf("a loss window inside a fault span was not attributed to it:\n%s", out)
	}
	if !strings.Contains(out, "| t+00:50 | pf-vip | 1.0 s | after tick 1 frr-restart → gateway-2 |") {
		t.Fatalf("a loss window past the convergence was not marked as after the fault:\n%s", out)
	}
}

func TestRenderReportListsTheSkippedDecisions(t *testing.T) {
	t.Parallel()
	events := []event{
		{TS: "2026-07-16T19:00:10Z", Event: evDecision, Tick: 1, Action: "frr-route-drop",
			Target: "gateway-1", Executed: boolPtr(false), SkipReason: "not-applicable"},
		{TS: "2026-07-16T19:00:30Z", Event: evDecision, Tick: 2, Action: "frr-restart",
			Target: "gateway-2", Executed: boolPtr(true)},
	}

	out := renderToString(t, reportRecord(t), events)

	if !strings.Contains(out, "| 1 | frr-route-drop | gateway-1 | not-applicable |") {
		t.Fatalf("the skipped decision and its reason are missing:\n%s", out)
	}
	// An executed decision carries no skip reason, so a leak would render
	// as this dashed row. (The bare tick/action/target prefix also occurs
	// in the recoveries table, so it cannot be asserted on.)
	if strings.Contains(out, "| 2 | frr-restart | gateway-2 | — |") {
		t.Fatalf("an executed decision leaked into the skipped table:\n%s", out)
	}
}

// Without a journal the loss table falls back to the record's 10-second
// buckets: coarser, but still localizing the loss in time.
func TestRenderReportFallsBackToTheLossBuckets(t *testing.T) {
	t.Parallel()

	out := renderToString(t, reportRecord(t), nil)

	if !strings.Contains(out, "| t+01:00–01:10 | fip-vm1 | 4/10 |") {
		t.Fatalf("the bucket fallback is missing the non-zero bucket:\n%s", out)
	}
	if strings.Contains(out, "| pf-vip | 0/") {
		t.Fatalf("a zero bucket leaked into the loss table:\n%s", out)
	}
}

func TestRenderReportSaysWhenNothingWasLost(t *testing.T) {
	t.Parallel()
	rec := reportRecord(t)
	rec.Probes = map[string]probeSummary{
		"pf-vip": {Target: "http://192.0.2.50:80/", Sent: 180,
			Buckets: []lossBucket{{OffsetMS: 0, Sent: 180}}},
	}

	out := renderToString(t, rec, nil)

	if !strings.Contains(out, "No probe loss was recorded.") {
		t.Fatalf("a lossless run did not say so:\n%s", out)
	}
}

// The renderer's writes are individually unchecked on purpose; the first
// failure still has to reach the exit code, or half a report on a full
// disk masquerades as a whole one.
func TestRunReportSurfacesAWriteError(t *testing.T) {
	t.Parallel()
	dir := writeRunDir(t, reportRecord(t), nil)

	err := runReport(dir, failingWriter{}, &fakeCommander{})

	if err == nil || !strings.Contains(err.Error(), "write the report") {
		t.Fatalf("error = %v, want the write failure surfaced", err)
	}
}

// writeRunDir lays a record (and optionally a journal) out on disk the
// way a finished run leaves them.
func writeRunDir(t *testing.T, rec *runRecord, journal []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeRecord(rec, filepath.Join(dir, summaryFile)); err != nil {
		t.Fatalf("write the record: %v", err)
	}
	if journal != nil {
		if err := os.WriteFile(filepath.Join(dir, journalFile), journal, 0o644); err != nil {
			t.Fatalf("write the journal: %v", err)
		}
	}
	return dir
}

func TestRunReportReadsADirectoryOrARecordFile(t *testing.T) {
	t.Parallel()
	events := []event{
		{TS: "2026-07-16T19:00:10Z", Event: evDecision, Tick: 1, Action: "frr-route-drop",
			Target: "gateway-1", Executed: boolPtr(false), SkipReason: "not-applicable"},
	}
	dir := writeRunDir(t, reportRecord(t), journalFor(t, events))

	for name, arg := range map[string]string{
		"the run directory": dir,
		"the record itself": filepath.Join(dir, summaryFile),
	} {
		var buf bytes.Buffer
		if err := runReport(arg, &buf, &fakeCommander{}); err != nil {
			t.Fatalf("runReport(%s): %v", name, err)
		}
		// The journal next to the record is picked up on both paths — the
		// skipped-decision table only exists when it was read.
		if !strings.Contains(buf.String(), "not-applicable") {
			t.Fatalf("runReport(%s) did not read the journal next to the record:\n%s", name, buf.String())
		}
	}
}

// A truncated last line is what an interrupted run leaves behind; the
// journal's intact prefix still has to enrich the report.
func TestRunReportSurvivesATruncatedJournal(t *testing.T) {
	t.Parallel()
	events := []event{
		{TS: "2026-07-16T19:00:10Z", Event: evDecision, Tick: 1, Action: "frr-route-drop",
			Target: "gateway-1", Executed: boolPtr(false), SkipReason: "not-applicable"},
	}
	journal := append(journalFor(t, events), []byte(`{"ts":"2026-07-16T19:00:20Z","event":"dec`)...)
	dir := writeRunDir(t, reportRecord(t), journal)
	var buf bytes.Buffer

	if err := runReport(dir, &buf, &fakeCommander{}); err != nil {
		t.Fatalf("runReport: %v", err)
	}

	if !strings.Contains(buf.String(), "not-applicable") {
		t.Fatalf("the intact journal prefix was thrown away:\n%s", buf.String())
	}
}

func TestRunReportRejectsWhatItCannotRender(t *testing.T) {
	t.Parallel()

	t.Run("a directory without a record", func(t *testing.T) {
		t.Parallel()
		err := runReport(t.TempDir(), &bytes.Buffer{}, &fakeCommander{})
		if err == nil || !strings.Contains(err.Error(), summaryFile) {
			t.Fatalf("error = %v, want one naming the missing %s", err, summaryFile)
		}
	})

	t.Run("a record with a foreign schema", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), summaryFile)
		if err := os.WriteFile(path, []byte(`{"schema":"something-else/v9"}`), 0o644); err != nil {
			t.Fatalf("write the record: %v", err)
		}
		err := runReport(path, &bytes.Buffer{}, &fakeCommander{})
		if err == nil || !strings.Contains(err.Error(), recordSchema) {
			t.Fatalf("error = %v, want one naming the expected schema %s", err, recordSchema)
		}
	})

	t.Run("a URL that is not an Actions run", func(t *testing.T) {
		t.Parallel()
		err := runReport("https://github.com/osism/ovn-network-agent/pull/5", &bytes.Buffer{}, &fakeCommander{})
		if err == nil || !strings.Contains(err.Error(), "not an Actions run URL") {
			t.Fatalf("error = %v, want the URL-shape rejection", err)
		}
	})
}

// The URL path shells out to `gh run download` through the commander
// seam; the fake plays gh and lays two artifacts out in the -D directory,
// the way a nightly's per-profile matrix uploads them.
func TestRunReportDownloadsAnActionsRun(t *testing.T) {
	t.Parallel()
	respond := func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		if !strings.HasPrefix(line, "gh run download 12345 -R osism/ovn-network-agent -D ") {
			t.Errorf("unexpected download argv: %v", argv)
			return "", errBoom
		}
		dest := argv[len(argv)-1]
		for _, profile := range []string{"everything-on", "pf-only"} {
			rec := reportRecord(t)
			rec.Inputs.Profile = profile
			dir := filepath.Join(dest, "e2e-artifacts-chaos-"+profile+"-12345-1")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			if err := writeRecord(rec, filepath.Join(dir, summaryFile)); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	var buf bytes.Buffer

	err := runReport("https://github.com/osism/ovn-network-agent/actions/runs/12345",
		&buf, &fakeCommander{respond: respond})
	if err != nil {
		t.Fatalf("runReport: %v", err)
	}
	out := buf.String()

	// One report per artifact, labeled with the artifact directory so the
	// six nightly records stay tellable-apart, in sorted (stable) order.
	first := strings.Index(out, "Artifact: `e2e-artifacts-chaos-everything-on-12345-1`")
	second := strings.Index(out, "Artifact: `e2e-artifacts-chaos-pf-only-12345-1`")
	if first == -1 || second == -1 || second < first {
		t.Fatalf("the run's records are missing or out of order:\n%s", out)
	}
	if got := strings.Count(out, "## Chaos run — "); got != 2 {
		t.Fatalf("rendered %d reports, want one per artifact record", got)
	}
}

func TestRunReportReportsAnEmptyActionsRun(t *testing.T) {
	t.Parallel()
	// gh succeeds but the run carried no chaos record at all — a run of
	// some other workflow, or one that died before the runner wrote one.
	err := runReport("https://github.com/osism/ovn-network-agent/actions/runs/99999",
		&bytes.Buffer{}, &fakeCommander{})

	if err == nil || !strings.Contains(err.Error(), "uploaded no "+summaryFile) {
		t.Fatalf("error = %v, want one saying the run held no record", err)
	}
}

// plannedRestartEvents is a journal with one of each planned-restart shape
// the report has to tell apart. The record around it ends at 19:03:30.
func plannedRestartEvents() []event {
	on, off, no, yes := boolPtr(true), boolPtr(false), boolPtr(false), boolPtr(true)
	return []event{
		// tick 1: a drained restart of gateway-1, with a short blip.
		{TS: "2026-07-16T19:00:10Z", Event: evInject, Tick: 1, Action: "gateway-restart", Target: "gateway-1", Drain: on},
		{TS: "2026-07-16T19:00:12Z", Event: evProbeTransition, Probe: "fip-vm2", Up: boolPtr(false)},
		{TS: "2026-07-16T19:00:12.400Z", Event: evProbeTransition, Probe: "fip-vm2", Up: boolPtr(true)},
		{TS: "2026-07-16T19:00:30Z", Event: evConverged, Tick: 1, Action: "gateway-restart", Target: "gateway-1"},
		// tick 2: a reloading flip on gateway-2, lossless.
		{TS: "2026-07-16T19:00:40Z", Event: evInject, Tick: 2, Action: "config-flip", Target: "gateway-2", Drain: on},
		{TS: "2026-07-16T19:00:41Z", Event: evConfigFlip, Target: "gateway-2", Flip: "cadence-toggle", Mode: flipModeReload, Rejected: no},
		{TS: "2026-07-16T19:00:45Z", Event: evConverged, Tick: 2, Action: "config-flip", Target: "gateway-2"},
		// tick 3: a flip the agent refused.
		{TS: "2026-07-16T19:00:50Z", Event: evInject, Tick: 3, Action: "config-flip", Target: "gateway-1", Drain: off},
		{TS: "2026-07-16T19:00:51Z", Event: evConfigFlip, Target: "gateway-1", Flip: "cidr-toggle", Mode: flipModeRestart, Rejected: yes},
		{TS: "2026-07-16T19:00:55Z", Event: evConverged, Tick: 3, Action: "config-flip", Target: "gateway-1"},
		// tick 4: a flip journaled before flips could reload — no mode.
		{TS: "2026-07-16T19:01:00Z", Event: evInject, Tick: 4, Action: "config-flip", Target: "gateway-2"},
		{TS: "2026-07-16T19:01:01Z", Event: evConfigFlip, Target: "gateway-2", Flip: "drain-toggle", Rejected: no},
		{TS: "2026-07-16T19:01:20Z", Event: evConverged, Tick: 4, Action: "config-flip", Target: "gateway-2"},
		// tick 5: a drained terminate of gateway-3 with a long loss window.
		{TS: "2026-07-16T19:01:30Z", Event: evInject, Tick: 5, Action: "agent-terminate", Target: "gateway-3", Drain: on},
		{TS: "2026-07-16T19:01:32Z", Event: evProbeTransition, Probe: "fip-vm1", Up: boolPtr(false)},
		{TS: "2026-07-16T19:01:43Z", Event: evProbeTransition, Probe: "fip-vm1", Up: boolPtr(true)},
		{TS: "2026-07-16T19:02:00Z", Event: evConverged, Tick: 5, Action: "agent-terminate", Target: "gateway-3"},
		// tick 6: an undrained restart that never converged, dark to the end.
		{TS: "2026-07-16T19:02:10Z", Event: evInject, Tick: 6, Action: "gateway-restart", Target: "gateway-2", Drain: off},
		{TS: "2026-07-16T19:02:12Z", Event: evProbeTransition, Probe: "fip-vm2", Up: boolPtr(false)},
	}
}

func TestRenderReportSplitsPlannedRestartsByDrain(t *testing.T) {
	t.Parallel()
	out := renderToString(t, reportRecord(t), plannedRestartEvents())

	for _, want := range []string{
		"### Planned restarts",
		"Drained restarts: 2 · longest loss window 11.0 s (fip-vm1)",
		"| 1 | gateway-restart | gateway-1 | restart | on | 400 ms (fip-vm2) |",
		"| 2 | config-flip | gateway-2 | reload | — | none |",
		"| 3 | config-flip | gateway-1 | rejected | — | none |",
		"| 4 | config-flip | gateway-2 | restart | — | none |",
		"| 5 | agent-terminate | gateway-3 | restart | on | 11.0 s (fip-vm1) |",
		"| 6 | gateway-restart | gateway-2 | restart | off | until run end (fip-vm2) |",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the Planned restarts section lacks %q:\n%s", want, out)
		}
	}
	// No gateway hosts the workloads, so none is set apart from the others.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "workload host") {
			t.Fatalf("the report still sets a workload host apart: %q\n%s", line, out)
		}
	}
}

// The summary reads only drained restarts; a run with none says so rather
// than reporting a hitless run it never measured.
func TestRenderReportSaysWhenNoDrainedRestartWasMeasured(t *testing.T) {
	t.Parallel()
	events := []event{
		{TS: "2026-07-16T19:00:10Z", Event: evInject, Tick: 1, Action: "gateway-restart", Target: "gateway-3", Drain: boolPtr(false)},
		{TS: "2026-07-16T19:00:20Z", Event: evConverged, Tick: 1, Action: "gateway-restart", Target: "gateway-3"},
		{TS: "2026-07-16T19:00:30Z", Event: evInject, Tick: 2, Action: "agent-terminate", Target: "gateway-1", Drain: boolPtr(false)},
		{TS: "2026-07-16T19:00:40Z", Event: evConverged, Tick: 2, Action: "agent-terminate", Target: "gateway-1"},
		// A flip that failed before it was journaled: no config-flip event
		// follows its inject, so nothing says how — or whether — it landed.
		{TS: "2026-07-16T19:00:50Z", Event: evInject, Tick: 3, Action: "config-flip", Target: "gateway-2", Drain: boolPtr(true)},
		{TS: "2026-07-16T19:01:00Z", Event: evInject, Tick: 4, Action: "controller-restart", Target: "gateway-1"},
	}

	out := renderToString(t, reportRecord(t), events)

	if !strings.Contains(out, "No drained restart in this run.") {
		t.Fatalf("a run without a drained restart did not say so:\n%s", out)
	}
	if !strings.Contains(out, "| 3 | config-flip | gateway-2 | — | — | none |") {
		t.Fatalf("a flip that never journaled how it landed was not shown as unknown:\n%s", out)
	}
}

// Without a planned restart in the journal — or without a journal at all —
// there is nothing to split, and the section stays out of the report.
func TestRenderReportLeavesOutPlannedRestartsItNeverSaw(t *testing.T) {
	t.Parallel()
	withoutRestarts := []event{
		{TS: "2026-07-16T19:00:30Z", Event: evInject, Tick: 1, Action: "frr-restart", Target: "gateway-2"},
		{TS: "2026-07-16T19:00:40Z", Event: evConverged, Tick: 1, Action: "frr-restart", Target: "gateway-2"},
	}
	for name, events := range map[string][]event{"no planned restart": withoutRestarts, "no journal": nil} {
		if out := renderToString(t, reportRecord(t), events); strings.Contains(out, "Planned restarts") {
			t.Fatalf("%s: the report rendered a Planned restarts section:\n%s", name, out)
		}
	}
}

// The VIP re-points table says in which phase the runner moved the
// port-forward VIP's routes. A failed re-point shows its error, and a
// journal written before re-points carried a phase still renders.
func TestRenderReportListsTheVIPRepointsByPhase(t *testing.T) {
	t.Parallel()
	events := []event{
		{TS: "2026-07-16T19:00:00Z", Event: evVIPRepoint, Phase: phaseStart, Target: "gateway-1"},
		{TS: "2026-07-16T19:00:30Z", Event: evInject, Tick: 1, Action: "gateway-kill", Target: "gateway-1"},
		{TS: "2026-07-16T19:00:31Z", Event: evVIPRepoint, Phase: phaseHold, Target: "gateway-2",
			Detail: "point 192.0.2.50 at gateway-2 on upstream: boom"},
		{TS: "2026-07-16T19:00:32Z", Event: evVIPRepoint, Phase: phaseHold, Target: "gateway-2"},
		{TS: "2026-07-16T19:01:10Z", Event: evVIPRepoint, Phase: phaseConverge, Target: "gateway-1"},
		{TS: "2026-07-16T19:02:00Z", Event: evVIPRepoint, Target: "gateway-3"},
	}

	out := renderToString(t, reportRecord(t), events)

	last := -1
	for _, want := range []string{
		"### VIP re-points",
		"| at | phase | owner | detail |",
		"| t+00:00 | start | gateway-1 | — |",
		"| t+00:31 | hold | gateway-2 | point 192.0.2.50 at gateway-2 on upstream: boom |",
		"| t+00:32 | hold | gateway-2 | — |",
		"| t+01:10 | converge | gateway-1 | — |",
		"| t+02:00 | — | gateway-3 | — |",
	} {
		at := strings.Index(out, want)
		if at < 0 {
			t.Fatalf("the VIP re-points section lacks %q:\n%s", want, out)
		}
		if at < last {
			t.Fatalf("%q is out of journal order:\n%s", want, out)
		}
		last = at
	}
}

// Without a re-point in the journal, or without a journal at all, there is
// nothing to list, and the section stays out of the report. A re-point
// whose timestamp does not parse cannot be placed in the run and gets no
// row.
func TestRenderReportLeavesOutVIPRepointsItNeverSaw(t *testing.T) {
	t.Parallel()
	withoutRepoints := []event{
		{TS: "2026-07-16T19:00:30Z", Event: evInject, Tick: 1, Action: "frr-restart", Target: "gateway-2"},
		{TS: "2026-07-16T19:00:40Z", Event: evConverged, Tick: 1, Action: "frr-restart", Target: "gateway-2"},
	}
	unplaceable := event{TS: "not-a-time", Event: evVIPRepoint, Phase: phaseHold, Target: "gateway-unplaceable"}
	for name, events := range map[string][]event{
		"no re-point":             withoutRepoints,
		"no journal":              nil,
		"only an unplaceable one": {unplaceable},
	} {
		if out := renderToString(t, reportRecord(t), events); strings.Contains(out, "VIP re-points") {
			t.Fatalf("%s: the report rendered a VIP re-points section:\n%s", name, out)
		}
	}

	out := renderToString(t, reportRecord(t), []event{
		unplaceable,
		{TS: "2026-07-16T19:00:32Z", Event: evVIPRepoint, Phase: phaseHold, Target: "gateway-2"},
	})

	if !strings.Contains(out, "| t+00:32 | hold | gateway-2 | — |") {
		t.Fatalf("the re-point with a readable timestamp is missing:\n%s", out)
	}
	if strings.Contains(out, "gateway-unplaceable") {
		t.Fatalf("a re-point with an unreadable timestamp got a row:\n%s", out)
	}
}

// The fault traces section puts a traced action's owner and path changes on
// the fault's own timeline: the baseline rows read `before`, every other row
// carries its time since the inject, and the tick's inject, restore and
// converged events sit between them in journal order. A baseline journaled
// after the inject, because the reading before it failed, carries its time
// too: it is not the state before the fault.
func TestRenderFaultTraces(t *testing.T) {
	t.Parallel()
	traced := []event{
		{TS: "2026-07-16T19:00:20Z", Event: evInject, Tick: 5, Action: "frr-restart", Target: "gateway-3"},
		{TS: "2026-07-16T19:00:25Z", Event: evConverged, Tick: 5, Action: "frr-restart", Target: "gateway-3"},
		{TS: "2026-07-16T19:00:59.8Z", Event: evCROwner, Tick: 6, Action: "double-failover",
			Object: "cr-lr0-public", To: "gateway-2", Detail: traceBaseline},
		{TS: "2026-07-16T19:00:59.9Z", Event: evUpstreamPath, Tick: 6, Action: "double-failover",
			Object: "192.0.2.10", To: "gateway-2", Detail: traceBaseline},
		{TS: "2026-07-16T19:01:00Z", Event: evInject, Tick: 6, Action: "double-failover",
			Target: "gateway-1", Peer: "gateway-2"},
		{TS: "2026-07-16T19:01:01.5Z", Event: evCROwner, Tick: 6, Action: "double-failover",
			Object: "cr-lr0-public", From: "gateway-2", To: traceUnbound},
		{TS: "2026-07-16T19:01:30Z", Event: evRestore, Tick: 6, Action: "double-failover", Target: "gateway-1"},
		{TS: "2026-07-16T19:01:45Z", Event: evConverged, Tick: 6, Action: "double-failover",
			Target: "gateway-1", Peer: "gateway-2"},
		{TS: "2026-07-16T19:02:00Z", Event: evInject, Tick: 7, Action: "double-failover", Target: "gateway-2"},
		{TS: "not-a-time", Event: evUpstreamPath, Tick: 7, Action: "double-failover",
			Object: "192.0.2.12", From: "gateway-2", To: traceNone},
		// Tick 8's owner read failed before the inject and succeeded after
		// gateway-2 had claimed the port; its path read succeeded in time.
		{TS: "2026-07-16T19:02:19.9Z", Event: evUpstreamPath, Tick: 8, Action: "double-failover",
			Object: "192.0.2.14", To: "gateway-3", Detail: traceBaseline},
		{TS: "2026-07-16T19:02:20Z", Event: evInject, Tick: 8, Action: "double-failover",
			Target: "gateway-3", Peer: "gateway-1"},
		{TS: "2026-07-16T19:02:21Z", Event: evCROwner, Tick: 8, Action: "double-failover",
			Object: "cr-lr1-public", To: "gateway-2", Detail: traceBaseline},
		// The journal was cut before tick 9's inject: its rows have no anchor.
		{TS: "2026-07-16T19:02:40Z", Event: evCROwner, Tick: 9, Action: "double-failover",
			Object: "cr-lr1-public", To: "gateway-3", Detail: traceBaseline},
		{TS: "2026-07-16T19:02:41Z", Event: evCROwner, Tick: 9, Action: "double-failover",
			Object: "cr-lr1-public", From: "gateway-3", To: traceAbsent},
	}

	out := renderToString(t, reportRecord(t), traced)

	last := -1
	for _, want := range []string{
		"### Fault traces\n",
		"#### Tick 6: double-failover, target gateway-1, peer gateway-2\n",
		"| at | event | object | from | to |",
		"| before | cr-owner | cr-lr0-public | — | gateway-2 |",
		"| before | upstream-path | 192.0.2.10 | — | gateway-2 |",
		"| +0 ms | inject | gateway-1 | — | — |",
		"| +1.5 s | cr-owner | cr-lr0-public | gateway-2 | unbound |",
		"| +30.0 s | restore | gateway-1 | — | — |",
		"| +45.0 s | converged | gateway-1 | — | — |",
		"#### Tick 7: double-failover, target gateway-2\n",
		"| — | upstream-path | 192.0.2.12 | gateway-2 | none |",
		"#### Tick 8: double-failover, target gateway-3, peer gateway-1\n",
		"| before | upstream-path | 192.0.2.14 | — | gateway-3 |",
		"| +0 ms | inject | gateway-3 | — | — |",
		"| +1.0 s | cr-owner | cr-lr1-public | — | gateway-2 |",
		"#### Tick 9\n",
		"| — | cr-owner | cr-lr1-public | — | gateway-3 |",
		"| — | cr-owner | cr-lr1-public | gateway-3 | absent |",
	} {
		at := strings.Index(out, want)
		if at < 0 {
			t.Fatalf("the fault traces section lacks %q:\n%s", want, out)
		}
		if at < last {
			t.Fatalf("%q is out of journal order:\n%s", want, out)
		}
		last = at
	}
	if got := strings.Count(out, "### Fault traces"); got != 1 {
		t.Fatalf("the section heading was rendered %d times:\n%s", got, out)
	}
	if strings.Contains(out, "#### Tick 5") {
		t.Fatalf("a tick without a trace event got a table:\n%s", out)
	}

	for name, events := range map[string][]event{
		"no trace event": traced[:2],
		"no journal":     nil,
	} {
		if out := renderToString(t, reportRecord(t), events); strings.Contains(out, "Fault traces") {
			t.Fatalf("%s: the report rendered a fault traces section:\n%s", name, out)
		}
	}
}

// The route drift table lists what the agent's route watch counted across
// each route drop, in tick order, so a watch repair can be told from a tick
// repair. A recovery that measured nothing gets no row.
func TestRenderReportListsTheRouteDriftDeltas(t *testing.T) {
	t.Parallel()
	rec := reportRecord(t)
	rec.Recoveries = append(rec.Recoveries,
		recoveryRecord{Tick: 4, Action: "frr-route-drop", Target: "gateway-2", BudgetMS: 10_000, ConvergedMS: 40,
			RouteDrift: &routeDrift{FRR: 1}},
		recoveryRecord{Tick: 3, Action: "kernel-route-drop", Target: "gateway-1", BudgetMS: 10_000, ConvergedMS: 60,
			RouteDrift: &routeDrift{Kernel: 2}},
		// The tick repaired this one: the watch counted nothing.
		recoveryRecord{Tick: 5, Action: "kernel-route-drop", Target: "gateway-3", BudgetMS: 10_000, ConvergedMS: 5_040,
			RouteDrift: &routeDrift{}},
	)

	out := renderToString(t, rec, nil)

	last := -1
	for _, want := range []string{
		"### Recoveries",
		"### Route drift seen by the agent",
		"| tick | action | target | kernel | frr |",
		"| 3 | kernel-route-drop | gateway-1 | 2 | 0 |",
		"| 4 | frr-route-drop | gateway-2 | 0 | 1 |",
		"| 5 | kernel-route-drop | gateway-3 | 0 | 0 |",
		"### Probes",
	} {
		at := strings.Index(out, want)
		if at < 0 {
			t.Fatalf("the report lacks %q:\n%s", want, out)
		}
		if at < last {
			t.Fatalf("%q is out of order:\n%s", want, out)
		}
		last = at
	}
	section := out[strings.Index(out, "### Route drift seen by the agent"):strings.Index(out, "### Probes")]
	for _, action := range []string{"mgmt-delay", "frr-restart"} {
		if strings.Contains(section, action) {
			t.Errorf("the route drift table lists %s, which measured no drift:\n%s", action, section)
		}
	}
}

// Without a recovery that measured drift there is nothing to list, and the
// section stays out of the report. That holds for a run without route drops
// and for a record written before route_drift existed, which unmarshals
// without the field.
func TestRenderReportLeavesOutRouteDriftItNeverMeasured(t *testing.T) {
	t.Parallel()
	const oldRecord = `{
  "schema": "chaos-run-record/v1",
  "result": "pass",
  "inputs": {"seed": 7, "profile": "flat-minimal", "duration_ms": 180000},
  "recoveries": [{
    "tick": 1, "action": "kernel-route-drop", "target": "gateway-1",
    "budget_ms": 60000, "converged_ms": 5040,
    "down_ms": {"fip-vm1": 3400}, "down_windows": {"fip-vm1": 1},
    "from_inject_ms": {"fip-vm1": 3400}, "from_restore_ms": {"fip-vm1": 3400},
    "cr_owner_after": "gateway-1"
  }]
}`
	var old runRecord
	if err := json.Unmarshal([]byte(oldRecord), &old); err != nil {
		t.Fatalf("unmarshal the old record: %v", err)
	}
	if len(old.Recoveries) != 1 || old.Recoveries[0].RouteDrift != nil {
		t.Fatalf("the old record unmarshalled as %+v, want one recovery without route_drift", old.Recoveries)
	}

	for name, rec := range map[string]*runRecord{
		"no route drop":                   reportRecord(t),
		"record written before the field": &old,
	} {
		out := renderToString(t, rec, nil)
		if strings.Contains(out, "Route drift seen by the agent") {
			t.Errorf("%s: the report rendered a route drift section:\n%s", name, out)
		}
		if !strings.Contains(out, "### Recoveries") {
			t.Errorf("%s: the report lost its recoveries section:\n%s", name, out)
		}
	}
}

// The OVS flow drift table lists what the agent's flow watch counted across
// each ovs-flow-drop, in tick order, right after the route drift table. A
// recovery that measured nothing gets no row.
func TestRenderReportListsTheFlowDriftDeltas(t *testing.T) {
	t.Parallel()
	rec := reportRecord(t)
	rec.Recoveries = append(rec.Recoveries,
		recoveryRecord{Tick: 7, Action: "ovs-flow-drop", Target: "gateway-2", BudgetMS: 10_000, ConvergedMS: 40,
			FlowDrift: &flowDrift{MACTweak: 2, Hairpin: 1}},
		recoveryRecord{Tick: 3, Action: "kernel-route-drop", Target: "gateway-1", BudgetMS: 10_000, ConvergedMS: 60,
			RouteDrift: &routeDrift{Kernel: 1}},
		// The tick repaired this one: the watch counted nothing.
		recoveryRecord{Tick: 6, Action: "ovs-flow-drop", Target: "gateway-3", BudgetMS: 10_000, ConvergedMS: 5_040,
			FlowDrift: &flowDrift{}},
	)

	out := renderToString(t, rec, nil)

	last := -1
	for _, want := range []string{
		"### Route drift seen by the agent",
		"| 3 | kernel-route-drop | gateway-1 | 1 | 0 |",
		"### OVS flow drift seen by the agent",
		"| tick | action | target | mactweak | hairpin |",
		"| 6 | ovs-flow-drop | gateway-3 | 0 | 0 |",
		"| 7 | ovs-flow-drop | gateway-2 | 2 | 1 |",
		"### Probes",
	} {
		at := strings.Index(out, want)
		if at < 0 {
			t.Fatalf("the report lacks %q:\n%s", want, out)
		}
		if at < last {
			t.Fatalf("%q is out of order:\n%s", want, out)
		}
		last = at
	}
	section := out[strings.Index(out, "### OVS flow drift seen by the agent"):strings.Index(out, "### Probes")]
	for _, action := range []string{"kernel-route-drop", "mgmt-delay", "frr-restart"} {
		if strings.Contains(section, action) {
			t.Errorf("the OVS flow drift table lists %s, which measured no flow drift:\n%s", action, section)
		}
	}
}

// Without a recovery that measured flow drift there is nothing to list, and
// the section stays out of the report. That holds for a run without
// ovs-flow-drop and for a record written before flow_drift existed, which
// unmarshals without the field.
func TestRenderReportLeavesOutFlowDriftItNeverMeasured(t *testing.T) {
	t.Parallel()
	const oldRecord = `{
  "schema": "chaos-run-record/v1",
  "result": "pass",
  "inputs": {"seed": 7, "profile": "flat-minimal", "duration_ms": 180000},
  "recoveries": [{
    "tick": 1, "action": "ovs-flow-drop", "target": "gateway-1",
    "budget_ms": 60000, "converged_ms": 11300,
    "down_ms": {"cross-fip": 11300}, "down_windows": {"cross-fip": 1},
    "from_inject_ms": {"cross-fip": 11300}, "from_restore_ms": {"cross-fip": 11300},
    "cr_owner_after": "gateway-1"
  }]
}`
	var old runRecord
	if err := json.Unmarshal([]byte(oldRecord), &old); err != nil {
		t.Fatalf("unmarshal the old record: %v", err)
	}
	if len(old.Recoveries) != 1 || old.Recoveries[0].FlowDrift != nil {
		t.Fatalf("the old record unmarshalled as %+v, want one recovery without flow_drift", old.Recoveries)
	}

	withRouteDriftOnly := reportRecord(t)
	withRouteDriftOnly.Recoveries = append(withRouteDriftOnly.Recoveries,
		recoveryRecord{Tick: 3, Action: "kernel-route-drop", Target: "gateway-1", BudgetMS: 10_000, ConvergedMS: 60,
			RouteDrift: &routeDrift{Kernel: 1}})

	for name, rec := range map[string]*runRecord{
		"no ovs-flow-drop":                reportRecord(t),
		"route drift only":                withRouteDriftOnly,
		"record written before the field": &old,
	} {
		out := renderToString(t, rec, nil)
		if strings.Contains(out, "OVS flow drift seen by the agent") {
			t.Errorf("%s: the report rendered an OVS flow drift section:\n%s", name, out)
		}
		if !strings.Contains(out, "### Recoveries") {
			t.Errorf("%s: the report lost its recoveries section:\n%s", name, out)
		}
	}
}
