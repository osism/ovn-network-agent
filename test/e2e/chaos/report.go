package main

// -report turns a recorded run back into something a human can read.
// The runner's two artifacts are machine-readable on purpose — the
// journal replays, the record diffs — but the first question a triager
// asks ("did it pass, and where did the traffic go?") should not require
// a jq session. The renderer answers it in GitHub-flavored Markdown, so
// the same output serves a terminal, a file and the Actions job summary
// the CI workflow appends it to.
//
// It reads what a run left behind and nothing else: summary.json is the
// report's spine, and journal.jsonl — when it sits next to it — adds
// what the record alone cannot say, the *when*: which fault a probe's
// loss window overlapped, and which decisions the guardrails skipped.
// A record without a journal still renders; the loss table then falls
// back to the record's 10-second buckets.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// runURLPattern matches the browser URL of one Actions run — the form a
// triager copies out of the address bar or a workflow notification. The
// owner, the repository and the run id are all in it, so a URL is the
// one argument that needs no checkout to resolve.
var runURLPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/actions/runs/(\d+)`)

// reportDownloadTimeout bounds the `gh run download`. The chaos record
// itself is a few kilobytes, but a failed run's artifact carries the
// lab-state bundle alongside it, and a nightly run has one artifact per
// profile — so this is minutes, not the commander's 30-second default.
const reportDownloadTimeout = 5 * time.Minute

// slowestRecoveries caps the recovery table. A 10-minute run executes
// ~a dozen actions and lists them all; an hours-long soak would bury
// the verdict under hundreds of sub-second rows nobody scrolls past.
const slowestRecoveries = 15

// mdWriter is the renderer's pen: it remembers the first write error so
// the dozens of table-row writes need no per-call checks, and the error
// still reaches the exit code. After a failure every later write is a
// no-op — half a report on a full disk should not masquerade as a whole
// one, and runReport turns the recorded error into its verdict.
type mdWriter struct {
	w   io.Writer
	err error
}

func (m *mdWriter) printf(format string, args ...any) {
	if m.err != nil {
		return
	}
	_, m.err = fmt.Fprintf(m.w, format, args...)
}

// runReport renders every run record the argument names. The argument is
// either an Actions run URL (the artifacts are fetched with `gh`, and a
// nightly run yields one record per profile), a directory a run wrote its
// artifacts into, or a summary.json itself.
//
// Rendering a failed run is a successful report: the exit code says
// whether the report could be produced, not what the run found — the
// run's own job already carries that verdict.
func runReport(arg string, w io.Writer, cmdr commander) error {
	paths, cleanup, err := resolveRecords(arg, cmdr)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return err
	}
	out := &mdWriter{w: w}
	for i, path := range paths {
		if i > 0 {
			out.printf("\n")
		}
		rec, events, err := loadRecord(path)
		if err != nil {
			return err
		}
		// Only a multi-record report labels its sources: the artifact
		// directory name carries the profile and the run id, which is
		// what tells six nightly records apart.
		source := ""
		if len(paths) > 1 {
			source = filepath.Base(filepath.Dir(path))
		}
		renderReport(out, rec, events, source)
	}
	if out.err != nil {
		return fmt.Errorf("write the report: %w", out.err)
	}
	return nil
}

// resolveRecords turns the -report argument into the summary.json paths
// to render, plus a cleanup for the temporary download directory when
// the argument was a URL.
func resolveRecords(arg string, cmdr commander) ([]string, func(), error) {
	if m := runURLPattern.FindStringSubmatch(arg); m != nil {
		return downloadRunRecords(cmdr, m[1], m[2], m[3])
	}
	// Any other URL is a mistake worth naming — a job URL or a PR URL
	// would otherwise fall through to os.Stat and report a baffling
	// "no such file".
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		return nil, nil, fmt.Errorf(
			"%s is not an Actions run URL (want https://github.com/<owner>/<repo>/actions/runs/<id>)", arg)
	}
	info, err := os.Stat(arg)
	if err != nil {
		return nil, nil, fmt.Errorf("read the run record: %w", err)
	}
	if !info.IsDir() {
		return []string{arg}, nil, nil
	}
	path := filepath.Join(arg, summaryFile)
	if _, err := os.Stat(path); err != nil {
		return nil, nil, fmt.Errorf("%s holds no %s: %w", arg, summaryFile, err)
	}
	return []string{path}, nil, nil
}

// downloadRunRecords fetches a run's artifacts with `gh run download`
// and returns every summary.json among them, sorted so the nightly
// matrix renders in a stable profile order.
func downloadRunRecords(cmdr commander, owner, repo, id string) ([]string, func(), error) {
	dir, err := os.MkdirTemp("", "chaos-report-")
	if err != nil {
		return nil, nil, fmt.Errorf("create a download directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if _, err := cmdr.run(context.Background(), "gh", "run", "download", id,
		"-R", owner+"/"+repo, "-D", dir); err != nil {
		return nil, cleanup, fmt.Errorf(
			"download the artifacts of run %s (is `gh` installed and authenticated?): %w", id, err)
	}
	var paths []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == summaryFile {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, cleanup, fmt.Errorf("scan the downloaded artifacts: %w", err)
	}
	if len(paths) == 0 {
		return nil, cleanup, fmt.Errorf(
			"run %s uploaded no %s — not a chaos run, or it died before the runner wrote one", id, summaryFile)
	}
	sort.Strings(paths)
	return paths, cleanup, nil
}

// loadRecord reads one run record and, best-effort, the journal next to
// it. The schema gate is deliberate: a summary.json from some other tool
// would render a report full of zeroes that reads like a clean run.
func loadRecord(path string) (*runRecord, []event, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read the run record: %w", err)
	}
	var rec runRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if rec.Schema != recordSchema {
		return nil, nil, fmt.Errorf("%s carries schema %q, want %q", path, rec.Schema, recordSchema)
	}
	return &rec, readJournal(filepath.Join(filepath.Dir(path), journalFile)), nil
}

// readJournal reads the event lines the report enriches itself with.
// Best-effort by design: a record without a journal renders from the
// buckets alone, and an interrupted run truncates its last line — a
// report that refused to render exactly the runs most worth triaging
// would be useless. Unparsable lines are skipped, not fatal.
func readJournal(path string) []event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var events []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			events = append(events, ev)
		}
	}
	return events
}

// renderReport writes one run's Markdown report. events may be nil.
func renderReport(w *mdWriter, rec *runRecord, events []event, source string) {
	verdict := "✅ pass"
	if rec.Result != resultPass {
		verdict = "❌ " + rec.Result
	}
	w.printf("## Chaos run — %s — profile `%s`, seed %d\n\n",
		verdict, rec.Inputs.Profile, rec.Inputs.Seed)
	if source != "" {
		w.printf("Artifact: `%s`\n\n", source)
	}

	start, startOK := parseTS(rec.StartedAt)
	end, endOK := parseTS(rec.EndedAt)
	wall := "unknown"
	if startOK && endOK {
		wall = end.Sub(start).Round(time.Second).String()
	}
	w.printf("%d ticks — %d executed, %d skipped · %d violations · %d baseline sweeps (%d dual-claim, %d errors) · wall clock %s\n\n",
		rec.Ticks, rec.Decisions.Executed, rec.Decisions.Skipped, len(rec.Violations),
		rec.Checks.Sweeps, rec.Checks.DualClaim, rec.Checks.Errors, wall)
	if line := actionsLine(rec.ActionsByName); line != "" {
		w.printf("Faults injected: %s\n\n", line)
	}
	w.printf("Replay: `make e2e-chaos CHAOS_FLAGS=\"%s\"`\n\n", replayFlags(rec.Inputs))

	renderViolations(w, rec)
	renderRecoveries(w, rec)
	renderProbes(w, rec)
	renderLoss(w, rec, events, start, end)
	renderSettles(w, rec)
	renderSkipped(w, events)
	renderInputs(w, rec)
}

// actionsLine is the injected-fault histogram as one line, most frequent
// first — a run's fingerprint at a glance, without a table per action.
func actionsLine(byName map[string]int) string {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if byName[names[i]] != byName[names[j]] {
			return byName[names[i]] > byName[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("`%s` ×%d", name, byName[name]))
	}
	return strings.Join(parts, ", ")
}

// replayFlags reconstructs the CHAOS_FLAGS that replay this run — the
// full reproducibility contract except -weights, which the record stores
// already resolved (defaults folded in), so the overriding spec cannot
// be read back out of it. The inputs block at the bottom of the report
// carries the resolved weights for a manual comparison.
func replayFlags(in runInputs) string {
	return fmt.Sprintf("-seed %d -profile %s -duration %s -tick-min %s -tick-max %s -settle-every %s -settle-timeout %s",
		in.Seed, in.Profile, msDuration(in.DurationMS), msDuration(in.TickMinMS), msDuration(in.TickMaxMS),
		msDuration(in.SettleEveryMS), msDuration(in.SettleTimeoutMS))
}

func renderViolations(w *mdWriter, rec *runRecord) {
	if len(rec.Violations) == 0 {
		return
	}
	w.printf("### Violations\n\n")
	w.printf("| kind | tick | action | target | detail | journal line |\n")
	w.printf("| --- | --- | --- | --- | --- | --- |\n")
	for _, v := range rec.Violations {
		w.printf("| %s | %s | %s | %s | %s | %s |\n",
			cell(v.Kind), orDash(v.Tick), orDashS(v.Action), orDashS(v.Target),
			cell(v.Detail), orDash(v.JournalOffset))
	}
	w.printf("\n")
}

func renderRecoveries(w *mdWriter, rec *runRecord) {
	if len(rec.Recoveries) == 0 {
		return
	}
	rows := make([]recoveryRecord, len(rec.Recoveries))
	copy(rows, rec.Recoveries)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ConvergedMS > rows[j].ConvergedMS })
	title := "### Recoveries — slowest first"
	if len(rows) > slowestRecoveries {
		title = fmt.Sprintf("### Recoveries — slowest %d of %d", slowestRecoveries, len(rows))
		rows = rows[:slowestRecoveries]
	}
	w.printf("%s\n\n", title)
	w.printf("| tick | action | target | converged | budget | probe loss after restore |\n")
	w.printf("| --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		w.printf("| %d | %s | %s | %s | %s | %s |\n",
			r.Tick, cell(r.Action), cell(r.Target),
			fmtMS(r.ConvergedMS), fmtMS(r.BudgetMS), probeLoss(r.FromRestoreMS))
	}
	w.printf("\n")
}

// probeLoss names the probes an action actually hurt, measured from the
// restore — the anchor the recovery budget is enforced against, because
// resources pinned to the node under fault are legitimately dark while
// it is held down.
func probeLoss(fromRestore map[string]int64) string {
	names := make([]string, 0, len(fromRestore))
	for name, ms := range fromRestore {
		if ms > 0 {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %s", name, fmtMS(fromRestore[name])))
	}
	return strings.Join(parts, ", ")
}

func renderProbes(w *mdWriter, rec *runRecord) {
	if len(rec.Probes) == 0 {
		return
	}
	names := make([]string, 0, len(rec.Probes))
	for name := range rec.Probes {
		names = append(names, name)
	}
	sort.Strings(names)
	w.printf("### Probes\n\n")
	w.printf("| probe | target | sent | lost | loss | transitions |\n")
	w.printf("| --- | --- | --- | --- | --- | --- |\n")
	for _, name := range names {
		p := rec.Probes[name]
		loss := "—"
		if p.Sent > 0 {
			loss = fmt.Sprintf("%.1f%%", float64(p.Lost)*100/float64(p.Sent))
		}
		w.printf("| %s | %s | %d | %d | %s | %d |\n",
			cell(name), cell(p.Target), p.Sent, p.Lost, loss, p.Transitions)
	}
	w.printf("\n")
}

// renderLoss is the report's "where did the traffic go" section. With a
// journal it renders one row per down window, each attributed to the
// fault whose inject→converged span it overlapped; without one it falls
// back to the record's 10-second buckets, which localize the loss in
// time but cannot name the fault.
func renderLoss(w *mdWriter, rec *runRecord, events []event, start, end time.Time) {
	spans := lossSpans(events)
	if len(spans) > 0 {
		faults := faultSpans(events, end)
		w.printf("### Loss windows\n\n")
		w.printf("| at | probe | down for | during |\n")
		w.printf("| --- | --- | --- | --- |\n")
		for _, s := range spans {
			down := "until run end"
			if !s.end.IsZero() {
				down = fmtMS(s.end.Sub(s.start).Milliseconds())
			}
			w.printf("| t+%s | %s | %s | %s |\n",
				offset(start, s.start), cell(s.probe), down, cell(duringFaults(s, faults, end)))
		}
		w.printf("\n")
		return
	}
	rows := lossBucketRows(rec)
	if len(rows) == 0 {
		w.printf("No probe loss was recorded.\n\n")
		return
	}
	w.printf("### Loss windows — 10 s buckets (no journal next to the record)\n\n")
	w.printf("| window | probe | lost/sent |\n")
	w.printf("| --- | --- | --- |\n")
	for _, r := range rows {
		w.printf("| t+%s–%s | %s | %d/%d |\n",
			offsetMS(r.offsetMS), offsetMS(r.offsetMS+10_000), cell(r.probe), r.lost, r.sent)
	}
	w.printf("\n")
}

func renderSettles(w *mdWriter, rec *runRecord) {
	if len(rec.Settles) == 0 {
		return
	}
	w.printf("### Settle windows\n\n")
	w.printf("| tick | converged | result | violations |\n")
	w.printf("| --- | --- | --- | --- |\n")
	for _, s := range rec.Settles {
		result := resultPass
		if !s.Passed {
			result = resultFail
		}
		w.printf("| %d | %s | %s | %d |\n", s.Tick, fmtMS(s.ConvergedMS), result, s.Violations)
	}
	w.printf("\n")
}

// renderSkipped lists the decisions the guardrails vetoed. Only the
// journal knows why: the record counts skips but does not keep their
// reasons.
func renderSkipped(w *mdWriter, events []event) {
	var rows []event
	for _, ev := range events {
		if ev.Event == evDecision && ev.Executed != nil && !*ev.Executed {
			rows = append(rows, ev)
		}
	}
	if len(rows) == 0 {
		return
	}
	w.printf("### Skipped decisions\n\n")
	w.printf("| tick | action | target | reason |\n")
	w.printf("| --- | --- | --- | --- |\n")
	for _, ev := range rows {
		w.printf("| %d | %s | %s | %s |\n",
			ev.Tick, cell(ev.Action), cell(ev.Target), orDashS(ev.SkipReason))
	}
	w.printf("\n")
}

// renderInputs folds the full replay contract — including the resolved
// weights the replay line cannot carry — behind a details block, so the
// report stays scannable but the contract stays one click away.
func renderInputs(w *mdWriter, rec *runRecord) {
	raw, err := json.MarshalIndent(rec.Inputs, "", "  ")
	if err != nil {
		return
	}
	w.printf("<details>\n<summary>Run inputs (the replay contract)</summary>\n\n```json\n%s\n```\n\n</details>\n", raw)
}

// faultSpan is one executed action's inject→converged window, read back
// out of the journal. A fault the run never saw converge (an aborted or
// interrupted run) stays open until runEnd.
type faultSpan struct {
	tick           int
	action, target string
	inject, end    time.Time
}

func faultSpans(events []event, runEnd time.Time) []faultSpan {
	var spans []faultSpan
	byTick := map[int]int{}
	for _, ev := range events {
		ts, ok := parseTS(ev.TS)
		if !ok {
			continue
		}
		switch ev.Event {
		case evInject:
			byTick[ev.Tick] = len(spans)
			spans = append(spans, faultSpan{tick: ev.Tick, action: ev.Action, target: ev.Target, inject: ts})
		case evConverged:
			if i, seen := byTick[ev.Tick]; seen {
				spans[i].end = ts
			}
		}
	}
	for i := range spans {
		if spans[i].end.IsZero() {
			spans[i].end = runEnd
		}
	}
	return spans
}

// lossSpan is one probe's down window, paired from its transition events.
// An open span (down at run end) keeps a zero end.
type lossSpan struct {
	probe      string
	start, end time.Time
}

func lossSpans(events []event) []lossSpan {
	open := map[string]time.Time{}
	var spans []lossSpan
	for _, ev := range events {
		if ev.Event != evProbeTransition || ev.Up == nil {
			continue
		}
		ts, ok := parseTS(ev.TS)
		if !ok {
			continue
		}
		if !*ev.Up {
			if _, already := open[ev.Probe]; !already {
				open[ev.Probe] = ts
			}
		} else if start, down := open[ev.Probe]; down {
			spans = append(spans, lossSpan{probe: ev.Probe, start: start, end: ts})
			delete(open, ev.Probe)
		}
	}
	for probe, start := range open {
		spans = append(spans, lossSpan{probe: probe, start: start})
	}
	sort.Slice(spans, func(i, j int) bool {
		if !spans[i].start.Equal(spans[j].start) {
			return spans[i].start.Before(spans[j].start)
		}
		return spans[i].probe < spans[j].probe
	})
	return spans
}

// duringFaults names the faults a loss window overlapped. A window that
// overlapped none — loss that surfaced only after the engine declared
// convergence, like a VIP repoint racing the prober — is attributed to
// the most recent fault before it, marked "after", rather than dropped:
// unattributed loss is exactly the kind a triager must not overlook.
func duringFaults(s lossSpan, faults []faultSpan, runEnd time.Time) string {
	end := s.end
	if end.IsZero() {
		end = runEnd
	}
	var hits []string
	for _, f := range faults {
		if s.start.Before(f.end) && end.After(f.inject) {
			hits = append(hits, fmt.Sprintf("tick %d %s → %s", f.tick, f.action, f.target))
		}
	}
	if len(hits) > 0 {
		return strings.Join(hits, "; ")
	}
	last := -1
	for i, f := range faults {
		if f.inject.Before(s.start) {
			last = i
		}
	}
	if last >= 0 {
		f := faults[last]
		return fmt.Sprintf("after tick %d %s → %s", f.tick, f.action, f.target)
	}
	return "—"
}

// lossBucketRow is one non-zero bucket of the record's per-probe loss
// series — the journal-less fallback for the loss table.
type lossBucketRow struct {
	offsetMS   int64
	probe      string
	lost, sent int
}

func lossBucketRows(rec *runRecord) []lossBucketRow {
	var rows []lossBucketRow
	for name, p := range rec.Probes {
		for _, b := range p.Buckets {
			if b.Lost > 0 {
				rows = append(rows, lossBucketRow{offsetMS: b.OffsetMS, probe: name, lost: b.Lost, sent: b.Sent})
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].offsetMS != rows[j].offsetMS {
			return rows[i].offsetMS < rows[j].offsetMS
		}
		return rows[i].probe < rows[j].probe
	})
	return rows
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// fmtMS renders a duration the way a reader compares them: milliseconds
// stay milliseconds, seconds get one decimal, anything longer reads as a
// Go duration.
func fmtMS(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%d ms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1f s", float64(ms)/1000)
	default:
		return (time.Duration(ms) * time.Millisecond).Round(time.Second).String()
	}
}

func msDuration(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }

// offset renders a timestamp as mm:ss from the run start — the same
// clock the record's buckets use, so the two loss tables line up.
func offset(start, t time.Time) string {
	d := t.Sub(start)
	if d < 0 {
		d = 0
	}
	return offsetMS(d.Milliseconds())
}

func offsetMS(ms int64) string {
	total := (ms + 500) / 1000
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}

// cell makes a value safe inside a Markdown table: a pipe or a newline
// in a violation detail must not shear the row apart.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}

func orDash(n int) string {
	if n == 0 {
		return "—"
	}
	return fmt.Sprintf("%d", n)
}

func orDashS(s string) string {
	if s == "" {
		return "—"
	}
	return cell(s)
}
