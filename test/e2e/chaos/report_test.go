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
				FromRestoreMS: map[string]int64{"fip-vm1": 0, "pf-vip": 0}},
			{Tick: 2, Action: "frr-restart", Target: "gateway-2", BudgetMS: 120_000, ConvergedMS: 5_200,
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
		"| 2 | frr-restart | gateway-2 | 5.2 s | 2m0s | fip-vm1 3.8 s |",
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
