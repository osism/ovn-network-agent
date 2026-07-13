package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Bad inputs must fail before a single fault is injected: a run built on
// a contradiction cannot be replayed and is not worth a lab.
func TestRunRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		tickMin  time.Duration
		tickMax  time.Duration
		weights  string
		wantErr  string
	}{
		{
			name:     "tick bounds inverted",
			duration: time.Minute,
			tickMin:  30 * time.Second,
			tickMax:  10 * time.Second,
			wantErr:  "below -tick-min",
		},
		// A tick interval near zero spins the loop and writes a decision
		// line per iteration: gigabytes of journal on a run that measures
		// nothing, because no fault fits between two ticks.
		{
			name:     "tick interval below the floor",
			duration: time.Minute,
			tickMin:  0,
			tickMax:  0,
			wantErr:  "below the 100ms floor",
		},
		{
			name:     "tick interval well below the floor but not zero",
			duration: time.Minute,
			tickMin:  time.Millisecond,
			tickMax:  5 * time.Millisecond,
			wantErr:  "below the 100ms floor",
		},
		// The inputs are journaled in milliseconds, so a sub-millisecond
		// duration would truncate to a zero-tick run reporting "pass".
		{
			name:     "duration below the floor",
			duration: 500 * time.Microsecond,
			tickMin:  10 * time.Second,
			tickMax:  30 * time.Second,
			wantErr:  "below the 1s floor",
		},
		{
			name:     "unknown action in the weights",
			duration: time.Minute,
			tickMin:  10 * time.Second,
			tickMax:  30 * time.Second,
			weights:  "gateway-melt=1",
			wantErr:  "unknown action",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()

			code, err := run(config{
				seed:       42,
				duration:   tc.duration,
				tickMin:    tc.tickMin,
				tickMax:    tc.tickMax,
				weightSpec: tc.weights,
				labName:    "ovn-e2e",
				outDir:     out,
				collect:    "/nonexistent",
			})

			if code != exitFatal {
				t.Fatalf("exit code = %d, want %d", code, exitFatal)
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.wantErr)
			}
			// Nothing was set up, so nothing was written.
			if entries, _ := os.ReadDir(out); len(entries) != 0 {
				t.Fatalf("a rejected run left artifacts behind: %v", entries)
			}
		})
	}
}

func TestWriteRecordProducesTheRunSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), summaryFile)
	rec := &runRecord{
		Inputs:     runInputs{Seed: 42, Lab: "ovn-e2e", Weights: map[string]int{"gateway-kill": 1}},
		Violations: []violationRecord{{Kind: violationRecoveryTimeout, Target: "gateway-2", Detail: "budget"}},
	}
	rec.finalize(time.Now())

	if err := writeRecord(rec, path); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var decoded runRecord
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("%s is not valid JSON: %v", summaryFile, err)
	}
	if decoded.Schema != recordSchema {
		t.Fatalf("schema = %q, want %q", decoded.Schema, recordSchema)
	}
	if decoded.Result != resultFail {
		t.Fatalf("result = %q, want %q — the run recorded a violation", decoded.Result, resultFail)
	}
	if decoded.Inputs.Seed != 42 || decoded.Inputs.Weights["gateway-kill"] != 1 {
		t.Fatalf("the record did not echo its inputs: %+v", decoded.Inputs)
	}
}

// The collector resolves its containers as clab-${LAB:-ovn-e2e}-<node>,
// and -lab is the runner's own name for the same lab. Without the
// hand-off, a run against a non-default lab dumps its bundle for
// containers that do not exist — and because every command in the
// collector is best-effort, it exits 0 and the runner reports a directory
// of empty files as collected lab state, on the exact run that produced a
// violation.
func TestCollectLabStateHandsTheLabNameToTheCollector(t *testing.T) {
	dir := t.TempDir()
	collector := filepath.Join(dir, "collect.sh")
	script := "#!/bin/sh\nmkdir -p \"$1\"\nprintf '%s' \"${LAB:-ovn-e2e}\" >\"$1/lab\"\n"
	if err := os.WriteFile(collector, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake collector: %v", err)
	}

	collectLabState(collector, "my-lab", dir)

	got, err := os.ReadFile(filepath.Join(dir, "lab-state", "lab"))
	if err != nil {
		t.Fatalf("the collector did not run: %v", err)
	}
	if string(got) != "my-lab" {
		t.Fatalf("the collector resolved containers for lab %q, want the runner's -lab", got)
	}
}

func TestWriteRecordReportsAnUnwritablePath(t *testing.T) {
	rec := &runRecord{}
	rec.finalize(time.Now())

	err := writeRecord(rec, filepath.Join(t.TempDir(), "no-such-dir", summaryFile))
	if err == nil {
		t.Fatal("a run record that could not be written was reported as written")
	}
}
