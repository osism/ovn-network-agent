package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The rejection names the floor, so a user who retries with exactly it gets a
// window that can converge. That makes the floor a promise, and it has to clear
// the arithmetic it is derived from: one slow-cadence reconcile to reach the
// first all-green evaluation, plus the confirmation gap a first green that
// fails its confirmation burns before the loop can try again.
func TestSettleTimeoutFloorOutlastsAReconcileAndAConfirmation(t *testing.T) {
	t.Parallel()
	slow, err := time.ParseDuration(slowCadence)
	if err != nil {
		t.Fatalf("parse slowCadence %q: %v", slowCadence, err)
	}
	if want := slow + settleConfirmDelay; minSettleTimeout < want {
		t.Fatalf("minSettleTimeout = %s, want >= %s (a %s reconcile plus the %s confirmation gap): "+
			"a run taking the advertised floor at face value times out into false violations",
			minSettleTimeout, want, slow, settleConfirmDelay)
	}
}

// Bad inputs must fail before a single fault is injected: a run built on
// a contradiction cannot be replayed and is not worth a lab.
func TestRunRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name          string
		profile       string
		duration      time.Duration
		tickMin       time.Duration
		tickMax       time.Duration
		settleEvery   time.Duration
		settleTimeout time.Duration
		weights       string
		wantErr       string
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
		// The profile decides the topology the run drives and the
		// configuration every agent in it runs — a run that cannot name
		// one cannot be replayed either.
		{
			name:     "unknown profile",
			profile:  "everything-off",
			duration: time.Minute,
			tickMin:  10 * time.Second,
			tickMax:  30 * time.Second,
			wantErr:  "unknown profile",
		},
		// A negative settle cadence is a contradiction like an inverted tick
		// bound: the run cannot schedule a settle window at all.
		{
			name:        "negative settle cadence",
			duration:    time.Minute,
			tickMin:     10 * time.Second,
			tickMax:     30 * time.Second,
			settleEvery: -time.Second,
			wantErr:     "-settle-every",
		},
		// A settle window shorter than the floor could not outlast the slow
		// reconcile a fault needs to converge, so every window would time out
		// into a false violation.
		{
			name:          "settle timeout below the floor",
			duration:      time.Minute,
			tickMin:       10 * time.Second,
			tickMax:       30 * time.Second,
			settleTimeout: 10 * time.Second,
			wantErr:       "below the 35s floor",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := t.TempDir()

			profile := tc.profile
			if profile == "" {
				profile = defaultProfileName
			}
			// A zero settle timeout is not the input under test, so it defaults
			// to a valid one — the cases above reject a specific contradiction,
			// not the absence of a settle flag.
			settleTimeout := tc.settleTimeout
			if settleTimeout == 0 {
				settleTimeout = 2 * time.Minute
			}
			code, err := run(config{
				seed:          42,
				profile:       profile,
				duration:      tc.duration,
				tickMin:       tc.tickMin,
				tickMax:       tc.tickMax,
				settleEvery:   tc.settleEvery,
				settleTimeout: settleTimeout,
				weightSpec:    tc.weights,
				labName:       "ovn-e2e",
				outDir:        out,
				collect:       "/nonexistent",
				gwnodeConfig:  "../gwnode-config.yaml",
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

// A run whose profile never lands is abandoned: the lab never reached the
// state the run measures against, so every fault after it would report a
// violation the lab did not cause. The abandoned run still has to say what
// went wrong — in its exit code, in its summary and in its journal —
// rather than report a "pass" over a lab it never configured.
func TestDriveAbandonsARunWhoseProfileNeverLands(t *testing.T) {
	tests := []struct {
		name    string
		respond func(argv []string) (string, error)
	}{
		// Every gateway's config carries that gateway's own management
		// address, so a run that cannot read one cannot render a config at
		// all.
		{
			name: "the management address the config needs cannot be read",
			respond: func(argv []string) (string, error) {
				if strings.Contains(strings.Join(argv, " "), "ip -o -4 addr show eth0") {
					return "", errBoom
				}
				return labWithConfig("stale config\n")(argv)
			},
		},
		{
			name: "the agent rejects the profile's configuration",
			respond: func(argv []string) (string, error) {
				if strings.Contains(strings.Join(argv, " "), "--check-config") {
					return "", errExit(t, 1)
				}
				return labWithConfig("stale config\n")(argv)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock()
			l := newTestLab(&fakeCommander{respond: tc.respond}, clock)
			p := testProfile(t, "pf-only")
			ap := newApplier(l, p, baseConfig(t))
			var buf bytes.Buffer
			ap.jrnl = newJournal(&buf, clock.now)
			rec := &runRecord{ActionsByName: map[string]int{}}

			code := drive(context.Background(), l, ap, allActions(p, l, ap, newChurner(l)), ap.jrnl, rec)

			if code != exitFatal {
				t.Fatalf("exit code = %d, want %d", code, exitFatal)
			}
			rec.finalize(clock.now())
			if rec.Result != resultFail || len(rec.Violations) != 1 ||
				rec.Violations[0].Kind != violationProfileApply {
				t.Fatalf("the summary of an abandoned run does not say what went wrong: %+v", rec)
			}
			if ev := lastEventOf(t, buf.String(), evViolation); ev.Kind != violationProfileApply {
				t.Fatalf("journaled %+v, want the violation the run was abandoned on", ev)
			}
		})
	}
}

// driveGreenResponses answers everything a full drive needs against a healthy
// lab: the oracle's queries (through the oracleLab fixture), the management
// address the applier discovers, and the live config it reads back — the baked
// one, so the default profile restarts nothing and the oracle primes and
// settles green.
func driveGreenResponses(t *testing.T, fx *oracleLab) func([]string) (string, error) {
	t.Helper()
	base := string(baseConfig(t))
	return func(argv []string) (string, error) {
		line := strings.Join(argv, " ")
		switch {
		case strings.Contains(line, "cat "+agentConfigPath):
			return base, nil
		case strings.Contains(line, "ip -o -4 addr show eth0"):
			return "172.20.20.4\n", nil
		// currentMaster resolves the cr-lr0-public owner UUID to a name with a
		// bare `list Chassis <uuid>`; the oracleLab fixture only models the
		// --format=json form, so answer the bare one with the master itself.
		case strings.Contains(line, "--bare") && strings.Contains(line, "list Chassis"):
			return "gateway-1\n", nil
		}
		return fx.respond(argv)
	}
}

func settleStartsIn(t *testing.T, journal string) int {
	t.Helper()
	n := 0
	for _, ev := range eventsIn(t, journal) {
		if ev.Event == evSettleStart {
			n++
		}
	}
	return n
}

// Every run ends with one final settle over its config-aware expected state —
// even a -settle-every of 0, where it is the only settle there is. An aborted
// run is the exception: it has already failed on the parking violation, and
// settling a lab with a dark node would only cascade.
func TestDriveRunsTheFinalSettle(t *testing.T) {
	t.Run("a green run ends with the final settle", func(t *testing.T) {
		fx := newOracleLab(t)
		clock := newFakeClock()
		l := newTestLab(&fakeCommander{respond: driveGreenResponses(t, fx)}, clock)
		p := defaultTestProfile(t)
		ap := newApplier(l, p, baseConfig(t))
		var buf bytes.Buffer
		ap.jrnl = newJournal(&buf, clock.now)
		// DurationMS 0 injects no fault, so the run is exactly its green start
		// state plus the one final settle the feature always runs.
		rec := &runRecord{
			Inputs:        runInputs{SettleTimeoutMS: (90 * time.Second).Milliseconds()},
			ActionsByName: map[string]int{},
		}

		code := drive(context.Background(), l, ap, nil, ap.jrnl, rec)
		rec.finalize(clock.now())

		if code != exitPass {
			t.Fatalf("exit code = %d, want %d: %+v", code, exitPass, rec.Violations)
		}
		if len(rec.Settles) != 1 || !rec.Settles[0].Passed {
			t.Fatalf("settles = %+v, want exactly one passing final settle", rec.Settles)
		}
		if ev := lastEventOf(t, buf.String(), evSettleResult); ev.Result != resultPass {
			t.Fatalf("the final settle-result was not a pass: %+v", ev)
		}
		if settleStartsIn(t, buf.String()) != 1 {
			t.Fatalf("journaled %d settle windows, want the one final settle", settleStartsIn(t, buf.String()))
		}
		if rec.Result != resultPass || len(rec.Violations) != 0 {
			t.Fatalf("a green run did not pass: %+v", rec)
		}
	})

	t.Run("an aborted run skips the final settle", func(t *testing.T) {
		fx := newOracleLab(t)
		clock := newFakeClock()
		l := newTestLab(&fakeCommander{respond: driveGreenResponses(t, fx)}, clock)
		p := defaultTestProfile(t)
		ap := newApplier(l, p, baseConfig(t))
		var buf bytes.Buffer
		ap.jrnl = newJournal(&buf, clock.now)
		rec := &runRecord{
			Inputs: runInputs{
				DurationMS:      time.Second.Milliseconds(),
				TickMinMS:       (100 * time.Millisecond).Milliseconds(),
				TickMaxMS:       (100 * time.Millisecond).Milliseconds(),
				SettleTimeoutMS: (90 * time.Second).Milliseconds(),
			},
			ActionsByName: map[string]int{},
		}
		// A fault the runner cannot undo parks its node and aborts the run.
		acts := noopActions("controller-restart")
		acts[0].holdMin, acts[0].holdMax = 0, 0
		acts[0].restore = func(context.Context, *lab, string) error { return errBoom }

		code := drive(context.Background(), l, ap, acts, ap.jrnl, rec)
		rec.finalize(clock.now())

		// drive returns exitPass even for a recorded violation — the fatal
		// codes are for setups it could not build, not for what a run found.
		if code != exitPass {
			t.Fatalf("exit code = %d, want %d", code, exitPass)
		}
		if rec.Result != resultFail {
			t.Fatalf("result = %q, want %q — the run parked a node", rec.Result, resultFail)
		}
		if len(rec.Settles) != 0 || settleStartsIn(t, buf.String()) != 0 {
			t.Fatalf("an aborted run still settled: settles=%+v, journal=%s", rec.Settles, buf.String())
		}
	})
}

// The oracle primes against the green start state before the first fault. A
// prime it cannot complete is a setup failure the run record cannot express as
// a lab fault, so the run is abandoned with exit 2 and an oracle-setup
// violation, like any other it could not be built on.
func TestDriveAbandonsWhenTheOracleCannotPrime(t *testing.T) {
	fx := newOracleLab(t)
	green := driveGreenResponses(t, fx)
	// The lab comes up green, but the oracle cannot read the Gateway_Chassis
	// table it primes its baseline from.
	respond := func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "list Gateway_Chassis") {
			return "", errBoom
		}
		return green(argv)
	}
	clock := newFakeClock()
	l := newTestLab(&fakeCommander{respond: respond}, clock)
	p := defaultTestProfile(t)
	ap := newApplier(l, p, baseConfig(t))
	var buf bytes.Buffer
	ap.jrnl = newJournal(&buf, clock.now)
	rec := &runRecord{
		Inputs:        runInputs{SettleTimeoutMS: (90 * time.Second).Milliseconds()},
		ActionsByName: map[string]int{},
	}

	code := drive(context.Background(), l, ap, nil, ap.jrnl, rec)

	if code != exitFatal {
		t.Fatalf("exit code = %d, want %d when the oracle cannot prime", code, exitFatal)
	}
	rec.finalize(clock.now())
	if rec.Result != resultFail || len(rec.Violations) != 1 ||
		rec.Violations[0].Kind != violationOracleSetup {
		t.Fatalf("the summary of a run abandoned at prime does not say so: %+v", rec)
	}
	if ev := lastEventOf(t, buf.String(), evViolation); ev.Kind != violationOracleSetup {
		t.Fatalf("journaled %+v, want the oracle-setup violation the run was abandoned on", ev)
	}
	// The final settle never runs on an abandoned setup.
	if settleStartsIn(t, buf.String()) != 0 {
		t.Fatalf("a run abandoned at prime still opened a settle window: %s", buf.String())
	}
}

func TestWriteRecordProducesTheRunSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), summaryFile)
	rec := &runRecord{
		Inputs: runInputs{
			Seed: 42, Profile: "pf-only", Lab: "ovn-e2e",
			Weights: map[string]int{"gateway-kill": 1},
		},
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
	// The profile is part of the reproducibility contract: a seed alone
	// does not identify a run, so the record has to carry both.
	if decoded.Inputs.Seed != 42 || decoded.Inputs.Profile != "pf-only" ||
		decoded.Inputs.Weights["gateway-kill"] != 1 {
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
