// chaos is a seeded chaos runner for the containerlab E2E lab (issue
// #176). It drives an already-deployed lab through a randomized fault
// sequence for a bounded duration and reports what it did.
//
// One run: apply the named configuration profile (issue #177) — the
// agent configuration every gateway runs, validated and rolled out
// without an image rebuild — layer the profile's scenario setups on top
// of the bootstrap baseline, start a continuous reachability probe from
// client-1, then tick until the duration expires: wait a randomized
// interval, pick a weighted action, check its guardrails, execute it,
// wait for the node to converge, record it.
//
// Reproducibility is the contract: the profile, the seed, the duration,
// the tick bounds and the action weights are the only inputs, and every
// decision the engine makes is drawn from a PCG stream seeded by -seed
// alone. Two runs with identical inputs replay the identical decision
// sequence — diff the `decision` lines of their journals to see it.
//
// Usage (against a lab that is already up — `make e2e-up`):
//
//	go run ./test/e2e/chaos [flags]
//	make e2e-chaos CHAOS_FLAGS="-duration 3m -seed 7"
//	make e2e-chaos CHAOS_FLAGS="-profile pf-only"
//
// Exit codes: 0 the run passed, 1 the run recorded a violation, 2 the
// runner could not set the run up. On a non-zero exit the lab's existing
// artifact collector is invoked into <out>/lab-state.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	exitPass       = 0
	exitViolations = 1
	exitFatal      = 2
)

// minTick is the floor under -tick-min. A tick interval near zero spins
// the loop as fast as the CPU allows and writes a decision line per
// iteration, so the journal grows without bound on a run that measures
// nothing: the faults never get a chance to land between two ticks.
const minTick = 100 * time.Millisecond

// minDuration is the floor under -duration. The run inputs are journaled
// (and the engine's clocks derived) in milliseconds, so a sub-millisecond
// duration would truncate to a zero-tick run reporting "pass".
const minDuration = time.Second

// collectTimeout bounds the lab-state collector: it runs against a lab
// that is already broken, where a `docker exec` can hang for good.
const collectTimeout = 2 * time.Minute

// config is one run's inputs, as parsed from the flags.
type config struct {
	seed         int64
	profile      string
	duration     time.Duration
	tickMin      time.Duration
	tickMax      time.Duration
	weightSpec   string
	labName      string
	outDir       string
	collect      string
	gwnodeConfig string
}

func main() {
	var cfg config
	flag.Int64Var(&cfg.seed, "seed", 42, "seed for every decision the engine makes")
	flag.StringVar(&cfg.profile, "profile", defaultProfileName,
		"configuration profile: the start topology and the agent configuration each gateway runs")
	flag.DurationVar(&cfg.duration, "duration", 10*time.Minute, "how long to keep injecting faults")
	flag.DurationVar(&cfg.tickMin, "tick-min", 10*time.Second, "lower bound of the interval between decisions")
	flag.DurationVar(&cfg.tickMax, "tick-max", 30*time.Second, "upper bound of the interval between decisions")
	flag.StringVar(&cfg.weightSpec, "weights", "", "override action weights, e.g. `gateway-kill=0,controller-restart=5`")
	flag.StringVar(&cfg.labName, "lab", "ovn-e2e", "containerlab lab name")
	flag.StringVar(&cfg.outDir, "out", "chaos-artifacts", "directory for the journal, the run record and the lab-state dump")
	flag.StringVar(&cfg.collect, "collect", "test/e2e/scenarios/collect-artifacts.sh",
		"lab-state collector, run into <out>/lab-state when the run does not pass")
	flag.StringVar(&cfg.gwnodeConfig, "gwnode-config", "test/e2e/gwnode-config.yaml",
		"the baked gateway agent config a profile's overlays are layered over")
	flag.Parse()

	code, err := run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[chaos] %v\n", err)
	}
	os.Exit(code)
}

func run(cfg config) (int, error) {
	if cfg.tickMin < minTick {
		return exitFatal, fmt.Errorf("-tick-min %s is below the %s floor", cfg.tickMin, minTick)
	}
	if cfg.tickMax < cfg.tickMin {
		return exitFatal, fmt.Errorf("-tick-max %s is below -tick-min %s", cfg.tickMax, cfg.tickMin)
	}
	if cfg.duration < minDuration {
		return exitFatal, fmt.Errorf("-duration %s is below the %s floor", cfg.duration, minDuration)
	}

	// The profile and the config it is layered over are resolved before a
	// single artifact is created: an unknown profile, like a bad weight,
	// is a contradiction in the inputs, and a run built on one cannot be
	// replayed and is not worth a lab.
	p, err := profileByName(cfg.profile)
	if err != nil {
		return exitFatal, err
	}
	base, err := os.ReadFile(cfg.gwnodeConfig)
	if err != nil {
		return exitFatal, fmt.Errorf("read the baked gateway config: %w", err)
	}

	actions := starterActions(p)
	weights, err := parseWeights(cfg.weightSpec, actions)
	if err != nil {
		return exitFatal, err
	}
	applyWeights(actions, weights)

	if err := os.MkdirAll(cfg.outDir, 0o755); err != nil {
		return exitFatal, fmt.Errorf("create %s: %w", cfg.outDir, err)
	}
	journalPath := filepath.Join(cfg.outDir, journalFile)
	jf, err := os.Create(journalPath)
	if err != nil {
		return exitFatal, fmt.Errorf("create %s: %w", journalPath, err)
	}
	defer func() { _ = jf.Close() }()

	// SIGINT/SIGTERM cancel the run; the summary is still written, so an
	// interrupted session is still triageable.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The first signal hands the signals straight back to their default
	// disposition, so a second one kills the runner. Cancelling only starts
	// the unwind: the engine still has to undo the fault it is holding, and
	// that restore is detached from the signal and can take minutes. An
	// operator who does not want to wait it out must have something better
	// than SIGKILL, which would lose summary.json entirely.
	go func() {
		<-ctx.Done()
		stop()
	}()

	started := time.Now()
	jrnl := newJournal(jf, time.Now)
	l := newLab(cfg.labName, execCommander{})
	rec := &runRecord{
		Inputs: runInputs{
			Seed:       cfg.seed,
			Profile:    p.name,
			DurationMS: cfg.duration.Milliseconds(),
			TickMinMS:  cfg.tickMin.Milliseconds(),
			TickMaxMS:  cfg.tickMax.Milliseconds(),
			Weights:    weights,
			Lab:        cfg.labName,
		},
		StartedAt:     started.UTC().Format(time.RFC3339Nano),
		ActionsByName: map[string]int{},
	}
	jrnl.emit(event{Event: evRunStart, Inputs: &rec.Inputs})

	code := drive(ctx, l, newApplier(l, p, base), actions, jrnl, rec)

	// The run is over and the engine has undone whatever it injected, so
	// the signals go back to their default disposition: everything below
	// only writes artifacts, and an operator who interrupts it must not
	// have to SIGKILL the runner to get out.
	stop()

	rec.finalize(time.Now())
	jrnl.emit(event{Event: evRunEnd, Result: rec.Result})
	if err := jrnl.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "[chaos] journal: %v\n", err)
	}
	if err := writeRecord(rec, filepath.Join(cfg.outDir, summaryFile)); err != nil {
		return exitFatal, err
	}
	if rec.Result == resultFail && code == exitPass {
		code = exitViolations
	}
	fmt.Fprintf(os.Stderr, "[chaos] %s: %d ticks, %d executed, %d skipped, %d violations (%s, %s)\n",
		rec.Result, rec.Ticks, rec.Decisions.Executed, rec.Decisions.Skipped,
		len(rec.Violations), journalPath, filepath.Join(cfg.outDir, summaryFile))
	if code != exitPass {
		collectLabState(cfg.collect, cfg.labName, cfg.outDir)
	}
	return code, nil
}

// drive puts the profile's configuration on the gateways, builds the
// start state, starts the prober and the baseline checks, and runs the
// engine to completion. It returns the exit code for a failure the run
// record cannot express (a configuration the agent rejected, a start
// state that never came up green).
func drive(ctx context.Context, l *lab, ap *applier, actions []*action, jrnl *journal, rec *runRecord) int {
	p := ap.profile
	if err := ap.discover(ctx); err != nil {
		return abandon(jrnl, rec, violationProfileApply, err)
	}
	if err := ap.applyProfile(ctx, jrnl); err != nil {
		return abandon(jrnl, rec, violationProfileApply, err)
	}
	if err := applyStartState(ctx, l, p); err != nil {
		return abandon(jrnl, rec, violationStartState, err)
	}
	jrnl.emit(event{Event: evStateApplied, Detail: p.layers()})

	// The prober and the baseline checks run for the whole run, alongside
	// the engine. Both are cancelled and then *waited for* before the run
	// record is read: a sweep still in flight would otherwise be
	// appending violations while the summary is being written.
	sideCtx, stopSide := context.WithCancel(ctx)
	defer stopSide()

	probes := newProber(l, p.probes, jrnl, time.Now)
	probes.start(sideCtx)

	e := newEngine(l, p, actions, probes, jrnl, rec)
	e.followMaster(ctx)

	checks := &baselineChecks{lab: l, engine: e}
	checksDone := make(chan struct{})
	go func() {
		defer close(checksDone)
		checks.run(sideCtx)
	}()

	e.run(ctx)

	stopSide()
	probes.stop()
	<-checksDone
	checks.finalize()
	rec.Probes = probes.summary()
	return exitPass
}

// abandon gives up on a run that could not be set up: the lab never
// reached the state the run measures against, so every fault after it
// would report a violation the lab did not cause. The failure is recorded
// and journaled like any other, so the summary of an abandoned run still
// says what went wrong.
func abandon(jrnl *journal, rec *runRecord, kind string, err error) int {
	rec.Violations = append(rec.Violations, violationRecord{
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Kind:   kind,
		Detail: err.Error(),
	})
	jrnl.emit(event{Event: evViolation, Kind: kind, Detail: err.Error()})
	fmt.Fprintf(os.Stderr, "[chaos] %s: %v\n", kind, err)
	return exitFatal
}

func writeRecord(rec *runRecord, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return rec.write(f)
}

// collectLabState reuses the lab's existing artifact collector so a
// failed chaos run leaves the same bundle every scenario leaves.
// Best-effort: a missing collector must not mask the run's own verdict.
//
// It only runs when the lab is already in a bad state, which is exactly
// when one of its `docker exec`s can wedge — hence the deadline. Without
// it the runner would hang with its exit code undelivered until CI kills
// the job, and no artifacts would be uploaded at all.
func collectLabState(collect, labName, outDir string) {
	dest := filepath.Join(outDir, "lab-state")
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, collect, dest)
	// The collector resolves its containers as clab-${LAB:-ovn-e2e}-<node>,
	// and -lab is the runner's own name for the same lab. Without it a run
	// against a non-default lab collects a bundle of empty files — every
	// command in the script is best-effort, so it still exits 0 and the
	// runner still reports the bundle as collected.
	cmd.Env = append(os.Environ(), "LAB="+labName)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[chaos] lab-state collection failed: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "[chaos] lab state collected into %s\n", dest)
}
