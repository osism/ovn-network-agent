package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestJournalWritesOneValidJSONObjectPerEvent(t *testing.T) {
	clock := newFakeClock()
	var buf bytes.Buffer
	j := newJournal(&buf, clock.now)

	j.emit(event{Event: evRunStart, Inputs: &runInputs{Seed: 42, Lab: "ovn-e2e"}})
	j.emit(event{Event: evDecision, Tick: 1, Action: "gateway-kill", Target: "gateway-2",
		IntervalMS: 12000, HoldMS: 20000, Executed: boolPtr(false), SkipReason: skipNoHealthyPeer})
	j.emit(event{Event: evProbeTransition, Probe: "pf-vip", Up: boolPtr(false)})

	if err := j.Err(); err != nil {
		t.Fatalf("journal: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("wrote %d lines for 3 events", len(lines))
	}
	for _, line := range lines {
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("line is not valid JSON: %v (%q)", err, line)
		}
		if raw["ts"] == "" || raw["event"] == "" {
			t.Fatalf("line is missing ts/event: %q", line)
		}
	}

	events := eventsIn(t, buf.String())
	// A skipped decision must record the skip and the values it drew:
	// they are what a replay is diffed on.
	d := events[1]
	if d.Executed == nil || *d.Executed {
		t.Fatalf("decision executed = %v, want false", d.Executed)
	}
	if d.SkipReason != skipNoHealthyPeer || d.IntervalMS != 12000 || d.HoldMS != 20000 {
		t.Fatalf("skipped decision lost its draw: %+v", d)
	}
	// `up: false` must survive the round trip — with a plain bool it
	// would be dropped by omitempty and a red probe would read as absent.
	if events[2].Up == nil || *events[2].Up {
		t.Fatalf("probe transition up = %v, want false", events[2].Up)
	}
}

// A journal that cannot be written must surface the error instead of
// pretending the run was recorded.
func TestJournalSurfacesWriteErrors(t *testing.T) {
	j := newJournal(failingWriter{}, newFakeClock().now)
	j.emit(event{Event: evRunStart})

	if err := j.Err(); err == nil {
		t.Fatal("a failed write was not reported")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errBoom }

// count() is the journal offset a violation is stamped with, so it must
// track exactly the lines a reader can find: zero on a fresh journal, one
// per successful emit, and left untouched by a write that never landed.
func TestJournalCountsEmittedLines(t *testing.T) {
	clock := newFakeClock()
	var buf bytes.Buffer
	j := newJournal(&buf, clock.now)

	if got := j.count(); got != 0 {
		t.Fatalf("a fresh journal counts %d lines, want 0", got)
	}

	j.emit(event{Event: evRunStart})
	j.emit(event{Event: evDecision, Tick: 1})
	j.emit(event{Event: evSettleStart, Tick: 1})
	if got := j.count(); got != 3 {
		t.Fatalf("count = %d after three emits, want 3", got)
	}

	// A write that never reached the writer must not advance the offset,
	// or a violation would be stamped with a line no reader can find.
	failing := newJournal(failingWriter{}, clock.now)
	failing.emit(event{Event: evRunStart})
	if got := failing.count(); got != 0 {
		t.Fatalf("a failed write advanced the offset to %d, want 0", got)
	}
}

func TestSummaryFailsOnViolation(t *testing.T) {
	clock := newFakeClock()

	clean := &runRecord{}
	clean.finalize(clock.now())
	if clean.Result != resultPass {
		t.Fatalf("result = %q, want %q", clean.Result, resultPass)
	}
	// The settle list must be an empty slice, never nil, so it serializes
	// as [] and a reader can tell "no settle windows ran" from "the field
	// is missing", exactly as the recovery and violation lists do.
	if clean.Settles == nil {
		t.Fatal("finalize left Settles nil; the record must carry [] not null")
	}

	dirty := &runRecord{Violations: []violationRecord{{Kind: violationDualClaim, Detail: "cr-lr0-public"}}}
	dirty.finalize(clock.now())
	if dirty.Result != resultFail {
		t.Fatalf("result = %q, want %q", dirty.Result, resultFail)
	}
	if dirty.Schema != recordSchema {
		t.Fatalf("schema = %q, want %q", dirty.Schema, recordSchema)
	}

	// A run whose only violations are the runner's own commands failing is
	// a harness fault, not an agent regression — the verdict says which of
	// the two a triager is looking at.
	harness := &runRecord{Violations: []violationRecord{
		{Kind: violationActionFailed, Detail: "remove the churn VIP entry"},
		{Kind: violationActionFailed, Detail: "restore gateway-2"},
	}}
	harness.finalize(clock.now())
	if harness.Result != resultHarnessFault {
		t.Fatalf("result = %q, want %q", harness.Result, resultHarnessFault)
	}

	// A product violation dominates: a run that hit both is a failed run.
	mixed := &runRecord{Violations: []violationRecord{
		{Kind: violationActionFailed, Detail: "restore gateway-2"},
		{Kind: violationRecoveryTimeout, Target: "gateway-2", Detail: "budget"},
	}}
	mixed.finalize(clock.now())
	if mixed.Result != resultFail {
		t.Fatalf("result = %q, want %q — a product violation dominates", mixed.Result, resultFail)
	}

	var buf bytes.Buffer
	if err := dirty.write(&buf); err != nil {
		t.Fatalf("write run record: %v", err)
	}
	var decoded runRecord
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("run record is not valid JSON: %v", err)
	}
	if decoded.Result != resultFail || len(decoded.Violations) != 1 {
		t.Fatalf("run record lost the violation: %+v", decoded)
	}
	if !strings.Contains(buf.String(), `"settles": []`) {
		t.Fatalf("an empty settle list serialized as null instead of []: %s", buf.String())
	}
}

// The journal offset points a violation at the decision line that caused
// it. A recorded offset must serialize; a zero one is the "no offset
// recorded" case, which omitempty keeps out of the record.
func TestViolationRecordSerializesTheJournalOffset(t *testing.T) {
	stamped, err := json.Marshal(violationRecord{Kind: violationDualClaim, JournalOffset: 7})
	if err != nil {
		t.Fatalf("marshal stamped violation: %v", err)
	}
	if !strings.Contains(string(stamped), `"journal_offset":7`) {
		t.Fatalf("the violation dropped its journal offset: %s", stamped)
	}

	unstamped, err := json.Marshal(violationRecord{Kind: violationDualClaim})
	if err != nil {
		t.Fatalf("marshal unstamped violation: %v", err)
	}
	if strings.Contains(string(unstamped), "journal_offset") {
		t.Fatalf("a zero journal offset leaked into the record: %s", unstamped)
	}
}

// The settle-window schedule is part of the reproducibility contract, so
// both keys must reach the run inputs verbatim — and, unlike the omitempty
// event fields, even at zero, or a replay cannot tell "not set" from "set
// to the value it computed".
func TestRunInputsSerializeTheSettleSchedule(t *testing.T) {
	set, err := json.Marshal(runInputs{SettleEveryMS: 5000, SettleTimeoutMS: 30000})
	if err != nil {
		t.Fatalf("marshal run inputs: %v", err)
	}
	for _, key := range []string{`"settle_every_ms":5000`, `"settle_timeout_ms":30000`} {
		if !strings.Contains(string(set), key) {
			t.Fatalf("the run inputs dropped %s: %s", key, set)
		}
	}

	zero, err := json.Marshal(runInputs{})
	if err != nil {
		t.Fatalf("marshal zero run inputs: %v", err)
	}
	for _, key := range []string{`"settle_every_ms":0`, `"settle_timeout_ms":0`} {
		if !strings.Contains(string(zero), key) {
			t.Fatalf("a zero settle schedule was dropped from the contract: %s", zero)
		}
	}
}

// The run record's "probe loss over time" is the per-target 10s buckets;
// recovery is dated from the moment the target came back.
func TestSummaryAggregatesLossBucketsAndRecoveries(t *testing.T) {
	clock := newFakeClock()
	p := newProber(nil, []probeTarget{{name: "pf-vip", kind: probeHTTP, addr: vipURL}},
		newJournal(&bytes.Buffer{}, clock.now), clock.now)

	anchor := clock.now()
	// 0–10s: green. 10–20s: the VIP goes dark. 20s: it comes back.
	for range 10 {
		p.record("pf-vip", true)
		clock.sleep(time.Second)
	}
	for range 10 {
		p.record("pf-vip", false)
		clock.sleep(time.Second)
	}
	p.record("pf-vip", true)

	sum := p.summary()["pf-vip"]
	if sum.Sent != 21 || sum.Lost != 10 {
		t.Fatalf("sent/lost = %d/%d, want 21/10", sum.Sent, sum.Lost)
	}
	if sum.Transitions != 2 {
		t.Fatalf("transitions = %d, want 2 (green→red→green)", sum.Transitions)
	}
	if len(sum.Buckets) != 3 {
		t.Fatalf("buckets = %+v, want three 10s slices", sum.Buckets)
	}
	if sum.Buckets[0].Lost != 0 || sum.Buckets[1].Lost != 10 || sum.Buckets[2].Lost != 0 {
		t.Fatalf("loss did not land in the middle bucket: %+v", sum.Buckets)
	}

	// The target went red after `anchor` and came back 20s later.
	if got := p.recoverySince(anchor)["pf-vip"]; got != 20_000 {
		t.Fatalf("recovery since the anchor = %dms, want 20000", got)
	}
	// A target that never went red after the anchor recovers in 0ms.
	if got := p.recoverySince(clock.now())["pf-vip"]; got != 0 {
		t.Fatalf("recovery for a target that stayed green = %dms, want 0", got)
	}
	if !p.allGreen() {
		t.Fatal("the target is up again but allGreen reports red")
	}
}

func TestProberReportsRedTargets(t *testing.T) {
	clock := newFakeClock()
	targets := []probeTarget{
		{name: "fip-vm1", kind: probePing, addr: "192.0.2.10"},
		{name: "pf-vip", kind: probeHTTP, addr: vipURL},
	}
	p := newProber(nil, targets, newJournal(&bytes.Buffer{}, clock.now), clock.now)

	p.record("fip-vm1", false)

	if p.allGreen() {
		t.Fatal("allGreen reports green with a red target")
	}
	if red := p.redTargets(); len(red) != 1 || red[0] != "fip-vm1" {
		t.Fatalf("redTargets = %v, want [fip-vm1]", red)
	}
}

// The flip fields describe a configuration change, and nothing else: on
// every other event they must stay out of the line entirely, or a journal
// reader cannot tell a decision that flipped a config from one that did
// not.
func TestFlipFieldsAreJournaledOnlyWhereTheyBelong(t *testing.T) {
	var buf bytes.Buffer
	j := newJournal(&buf, newFakeClock().now)

	j.emit(event{
		Event: evConfigFlip, Target: "gateway-2", Flip: "drain-toggle",
		From: "false", To: "true", Rejected: boolPtr(false),
	})
	j.emit(event{Event: evInject, Tick: 3, Action: "gateway-kill", Target: "gateway-1"})

	lines := eventsIn(t, buf.String())
	flip := lines[0]
	if flip.Flip != "drain-toggle" || flip.From != "false" || flip.To != "true" {
		t.Fatalf("the config-flip event lost its values: %+v", flip)
	}
	if flip.Rejected == nil || *flip.Rejected {
		t.Fatalf("an applied flip was journaled as rejected: %+v", flip)
	}
	raw := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(raw[0], `"rejected":false`) {
		t.Fatalf("an applied flip does not say so explicitly: %s", raw[0])
	}
	for _, field := range []string{"flip", "from", "to", "rejected"} {
		if strings.Contains(raw[1], `"`+field+`"`) {
			t.Fatalf("the %s field leaked into an event that never flipped anything: %s", field, raw[1])
		}
	}
}

// peer and object annotate the multi-node and object-touching faults. Like
// every other optional field they must stay out of the events that do not
// set them, so a reader can tell a pair fault from a single-node one and a
// drift fault from a container kill.
func TestPeerAndObjectAreJournaledOnlyWhereTheyBelong(t *testing.T) {
	var buf bytes.Buffer
	j := newJournal(&buf, newFakeClock().now)

	j.emit(event{
		Event: evDecision, Tick: 1, Action: "double-failover",
		Target: "gateway-1", Peer: "gateway-2", Executed: boolPtr(true),
	})
	j.emit(event{
		Event: evDecision, Tick: 2, Action: "kernel-route-drop",
		Target: "gateway-3", Object: "192.0.2.10/32 dev br-ex", Executed: boolPtr(true),
	})
	j.emit(event{Event: evInject, Tick: 3, Action: "gateway-kill", Target: "gateway-1"})

	events := eventsIn(t, buf.String())
	if events[0].Peer != "gateway-2" || events[0].Object != "" {
		t.Fatalf("the pair decision lost its peer or gained an object: %+v", events[0])
	}
	if events[1].Object != "192.0.2.10/32 dev br-ex" || events[1].Peer != "" {
		t.Fatalf("the drift decision lost its object or gained a peer: %+v", events[1])
	}
	raw := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for _, field := range []string{"peer", "object"} {
		if strings.Contains(raw[2], `"`+field+`"`) {
			t.Fatalf("the %s field leaked into an event that touched neither: %s", field, raw[2])
		}
	}
}

// The ovn-churn event carries what a churn touched and the values it moved
// between, so an add/remove cycle is legible from the journal.
func TestOVNChurnEventRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	j := newJournal(&buf, newFakeClock().now)

	j.emit(event{
		Event: evOVNChurn, Target: centralNode, Object: "fip 192.0.2.60",
		From: "absent", To: "192.0.2.60",
	})

	ev := eventsIn(t, buf.String())[0]
	if ev.Event != evOVNChurn || ev.Object != "fip 192.0.2.60" || ev.From != "absent" || ev.To != "192.0.2.60" {
		t.Fatalf("the ovn-churn event did not round-trip: %+v", ev)
	}
}
