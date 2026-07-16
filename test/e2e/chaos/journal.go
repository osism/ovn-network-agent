package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// The two machine-readable artifacts a run leaves behind:
//
//	<out>/journal.jsonl — one JSON object per decision and phase, in the
//	                      order the engine made them. Replaying a run
//	                      means diffing the `decision` lines of two runs.
//	<out>/summary.json  — the run record: inputs, what was executed,
//	                      probe loss over time, recovery durations,
//	                      violations.
const (
	journalFile  = "journal.jsonl"
	summaryFile  = "summary.json"
	recordSchema = "chaos-run-record/v1"
)

// Journal event names.
const (
	evRunStart        = "run-start"
	evProfileApply    = "profile-apply"
	evConfigFlip      = "config-flip"
	evStateApplied    = "state-applied"
	evDecision        = "decision"
	evInject          = "inject"
	evRestore         = "restore"
	evConverged       = "converged"
	evNodeState       = "node-state"
	evProbeTransition = "probe-transition"
	evVIPRepoint      = "vip-repoint"
	evOVNChurn        = "ovn-churn"
	evSettleStart     = "settle-start"
	evSettleResult    = "settle-result"
	evViolation       = "violation"
	evCheckError      = "check-error"
	evRunAborted      = "run-aborted"
	evRunEnd          = "run-end"
)

// event is one journal line. Every field is optional except ts/event, so
// one flat struct describes the whole event vocabulary and consumers can
// branch on `event`.
type event struct {
	TS          string           `json:"ts"`
	Event       string           `json:"event"`
	Tick        int              `json:"tick,omitempty"`
	Action      string           `json:"action,omitempty"`
	Target      string           `json:"target,omitempty"`
	Peer        string           `json:"peer,omitempty"`
	Object      string           `json:"object,omitempty"`
	IntervalMS  int64            `json:"interval_ms,omitempty"`
	HoldMS      int64            `json:"hold_ms,omitempty"`
	Executed    *bool            `json:"executed,omitempty"`
	SkipReason  string           `json:"skip_reason,omitempty"`
	Flip        string           `json:"flip,omitempty"`
	From        string           `json:"from,omitempty"`
	To          string           `json:"to,omitempty"`
	Rejected    *bool            `json:"rejected,omitempty"`
	Probe       string           `json:"probe,omitempty"`
	Up          *bool            `json:"up,omitempty"`
	State       string           `json:"state,omitempty"`
	CROwner     string           `json:"cr_owner,omitempty"`
	RecoveryMS  map[string]int64 `json:"recovery_ms,omitempty"`
	Kind        string           `json:"kind,omitempty"`
	Detail      string           `json:"detail,omitempty"`
	Result      string           `json:"result,omitempty"`
	ConvergedMS int64            `json:"converged_ms,omitempty"`
	Inputs      *runInputs       `json:"inputs,omitempty"`
}

// runInputs is the reproducibility contract: identical inputs replay the
// identical decision sequence. It is echoed into the `run-start` event
// and into the run record.
//
// The profile is one of them: it decides both the topology the run drives
// and the configuration every agent in it runs, so a seed without a
// profile does not identify a run.
type runInputs struct {
	Seed       int64  `json:"seed"`
	Profile    string `json:"profile"`
	DurationMS int64  `json:"duration_ms"`
	TickMinMS  int64  `json:"tick_min_ms"`
	TickMaxMS  int64  `json:"tick_max_ms"`
	// SettleEveryMS/SettleTimeoutMS are the settle-window schedule the
	// expected-state oracle runs on: how often the run pauses to let the
	// lab converge and how long it waits before calling that a failure.
	// They belong to the reproducibility contract because they change how
	// often the oracle is consulted, so a replay must see the same values.
	SettleEveryMS   int64          `json:"settle_every_ms"`
	SettleTimeoutMS int64          `json:"settle_timeout_ms"`
	Weights         map[string]int `json:"weights"`
	Lab             string         `json:"lab"`
}

// journal is the append-only JSONL writer. The prober writes transitions
// from its own goroutines, so writes are serialized.
type journal struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
	err error
	// n is the journal offset: the number of lines successfully emitted so
	// far. A later commit stamps a violation with this value so the oracle
	// can point at the offset of the last executed action.
	n int
}

func newJournal(w io.Writer, now func() time.Time) *journal {
	return &journal{w: w, now: now}
}

// emit appends one event. A write error is recorded and surfaced by
// Err() at the end of the run rather than aborting the tick loop: a full
// disk must not be reported as a lab failure.
func (j *journal) emit(e event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e.TS = j.now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(e)
	if err != nil {
		j.err = fmt.Errorf("marshal %s event: %w", e.Event, err)
		return
	}
	if _, err := fmt.Fprintf(j.w, "%s\n", line); err != nil {
		if j.err == nil {
			j.err = fmt.Errorf("write %s event: %w", e.Event, err)
		}
		return
	}
	// Only a line that both marshalled and reached the writer advances the
	// journal offset, so count() never claims a line a reader cannot find.
	j.n++
}

func (j *journal) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// count returns the journal offset — the number of lines emitted so far.
// A violation is stamped with this value to record which action it
// followed. It is safe to call from any goroutine.
func (j *journal) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.n
}

func boolPtr(b bool) *bool { return &b }

// lossBucket is one 10-second slice of a probe's history — the
// "probe loss over time" series in the run record.
type lossBucket struct {
	OffsetMS int64 `json:"offset_ms"`
	Sent     int   `json:"sent"`
	Lost     int   `json:"lost"`
}

type probeSummary struct {
	Target      string       `json:"target"`
	Sent        int          `json:"sent"`
	Lost        int          `json:"lost"`
	Transitions int          `json:"transitions"`
	Buckets     []lossBucket `json:"buckets"`
}

// recoveryRecord is how long each probe target took to come back after
// one action. Measured from two anchors: the fault injection and the
// restore. Resources pinned to the node under fault are legitimately
// dark while it is held down, so the restore anchor is the one the
// recovery budget is enforced against.
type recoveryRecord struct {
	Tick          int              `json:"tick"`
	Action        string           `json:"action"`
	Target        string           `json:"target"`
	Peer          string           `json:"peer,omitempty"`
	BudgetMS      int64            `json:"budget_ms"`
	ConvergedMS   int64            `json:"converged_ms"`
	FromInjectMS  map[string]int64 `json:"from_inject_ms"`
	FromRestoreMS map[string]int64 `json:"from_restore_ms"`
	CROwnerAfter  string           `json:"cr_owner_after"`
}

// settleRecord is the verdict of one settle window: how long the lab took
// to match the expected-state oracle, whether it matched at all, and how
// many invariants it broke while it did not. It is the run record's
// evidence that the lab was checked against its config, not just that no
// probe went red.
type settleRecord struct {
	Tick        int   `json:"tick"`
	ConvergedMS int64 `json:"converged_ms"`
	Passed      bool  `json:"passed"`
	Violations  int   `json:"violations"`
}

type violationRecord struct {
	TS     string `json:"ts"`
	Kind   string `json:"kind"`
	Tick   int    `json:"tick,omitempty"`
	Action string `json:"action,omitempty"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail"`
	// JournalOffset is the journal line count at the moment the violation
	// was recorded, i.e. the offset of the last executed action. It lets a
	// reader jump from a violation in the run record straight to the
	// decision in journal.jsonl that produced it.
	JournalOffset int `json:"journal_offset,omitempty"`
}

type decisionCounts struct {
	Total    int `json:"total"`
	Executed int `json:"executed"`
	Skipped  int `json:"skipped"`
}

// checkCounts is how much the baseline sweep actually did — the record's
// evidence that the invariants were evaluated at all, and not just that
// nothing was found. A run that could never reach SB and a run where the
// split-brain never happened are both "zero violations"; only these
// counts tell them apart.
type checkCounts struct {
	Sweeps    int `json:"sweeps"`
	DualClaim int `json:"dual_claim_evaluated"`
	Errors    int `json:"errors"`
}

// runRecord is <out>/summary.json.
type runRecord struct {
	Schema        string                  `json:"schema"`
	Inputs        runInputs               `json:"inputs"`
	StartedAt     string                  `json:"started_at"`
	EndedAt       string                  `json:"ended_at"`
	Result        string                  `json:"result"`
	Ticks         int                     `json:"ticks"`
	Decisions     decisionCounts          `json:"decisions"`
	Checks        checkCounts             `json:"checks"`
	ActionsByName map[string]int          `json:"actions_by_name"`
	Probes        map[string]probeSummary `json:"probes"`
	Recoveries    []recoveryRecord        `json:"recoveries"`
	Settles       []settleRecord          `json:"settles"`
	Violations    []violationRecord       `json:"violations"`
}

const (
	resultPass = "pass"
	resultFail = "fail"
)

// finalize stamps the result: any violation fails the run.
func (r *runRecord) finalize(endedAt time.Time) {
	r.Schema = recordSchema
	r.EndedAt = endedAt.UTC().Format(time.RFC3339Nano)
	r.Result = resultPass
	if len(r.Violations) > 0 {
		r.Result = resultFail
	}
	if r.ActionsByName == nil {
		r.ActionsByName = map[string]int{}
	}
	if r.Probes == nil {
		r.Probes = map[string]probeSummary{}
	}
	if r.Recoveries == nil {
		r.Recoveries = []recoveryRecord{}
	}
	if r.Settles == nil {
		r.Settles = []settleRecord{}
	}
	if r.Violations == nil {
		r.Violations = []violationRecord{}
	}
}

func (r *runRecord) write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("write run record: %w", err)
	}
	return nil
}
