package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// bootstrapDockerStub stands in for docker while bootstrap.sh runs. It
// records every call and runs no container. START_EXITS lists the exit
// code of each upstream zebra start attempt in order (attempts past the
// list exit 1), and START_STDERR is what a failing attempt prints.
// PKILL_EXIT is the exit code of the zebra kill between attempts: BusyBox
// pkill exits 1 when nothing matched, which is the crash case. PGREP_EXIT
// is the exit code of the end check's look. Every other call exits 0.
const bootstrapDockerStub = `#!/bin/sh
# docker: record argv; script the start attempts, the kill between them
# and the end check
printf '%s\n' "$*" >>"${DOCKER_CALLS}"
case "$*" in
*"--env ZEBRA_WAIT_SECS="*"sh -eu")
    cat >"${DOCKER_STDIN}"
    n=$(grep -c -e '--env ZEBRA_WAIT_SECS=.*sh -eu$' "${DOCKER_CALLS}")
    set -- ${START_EXITS}
    eval "code=\${$n:-1}"
    [ "${code}" -eq 0 ] || echo "${START_STDERR:-}" >&2
    exit "${code}" ;;
*"pkill -KILL -f ^/usr/lib/frr/zebra") exit "${PKILL_EXIT:-0}" ;;
*"pgrep -f ^/usr/lib/frr/zebra") exit "${PGREP_EXIT:-0}" ;;
esac
exit 0
`

// bootstrapSleepStub makes every pause in bootstrap.sh free.
const bootstrapSleepStub = `#!/bin/sh
# sleep: no-op, so the pause between attempts costs no test time
exit 0
`

// bootstrapRun is what one run of bootstrap.sh against the stubs left
// behind: its exit code, its stderr, the docker calls it made and the
// script the last upstream zebra start attempt fed to the container.
type bootstrapRun struct {
	code   int
	stderr string
	calls  []string
	stdin  string
}

// startCalls returns the recorded upstream zebra start attempts.
func (r bootstrapRun) startCalls() []string {
	var starts []string
	for _, call := range r.calls {
		if strings.Contains(call, "--env ZEBRA_WAIT_SECS=") && strings.HasSuffix(call, "sh -eu") {
			starts = append(starts, call)
		}
	}
	return starts
}

// count returns how often call was recorded verbatim.
func (r bootstrapRun) count(call string) int {
	n := 0
	for _, c := range r.calls {
		if c == call {
			n++
		}
	}
	return n
}

// lastLine returns the last line bootstrap.sh wrote to stderr.
func (r bootstrapRun) lastLine() string {
	text := strings.TrimRight(r.stderr, "\n")
	return text[strings.LastIndex(text, "\n")+1:]
}

// runBootstrap runs bash with bashArgs against the docker and sleep
// stubs. The environment is built from scratch, so a LAB_NAME or budget
// variable in the developer's shell cannot change the outcome; env adds
// to it.
func runBootstrap(t *testing.T, env []string, bashArgs ...string) bootstrapRun {
	t.Helper()
	stubs := t.TempDir()
	for name, body := range map[string]string{
		"docker": bootstrapDockerStub,
		"sleep":  bootstrapSleepStub,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s stub: %v", name, err)
		}
	}
	callsFile := filepath.Join(stubs, "docker-calls")
	stdinFile := filepath.Join(stubs, "docker-stdin")
	script := exec.Command("bash", bashArgs...)
	script.Env = append([]string{
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DOCKER_CALLS=" + callsFile,
		"DOCKER_STDIN=" + stdinFile,
	}, env...)
	var stderr bytes.Buffer
	script.Stderr = &stderr

	run := bootstrapRun{}
	if err := script.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run bash %q: %v", bashArgs, err)
		}
		run.code = exitErr.ExitCode()
	}
	run.stderr = stderr.String()
	// A file the stubs never wrote is an empty record.
	record := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read the stub record %s: %v", filepath.Base(path), err)
		}
		return string(raw)
	}
	if lines := strings.TrimSuffix(record(callsFile), "\n"); lines != "" {
		run.calls = strings.Split(lines, "\n")
	}
	run.stdin = record(stdinFile)
	return run
}

// runBootstrapFunc loads bootstrap.sh and calls fn, with env added to the
// stub environment.
func runBootstrapFunc(t *testing.T, fn string, env ...string) bootstrapRun {
	t.Helper()
	return runBootstrap(t, env, "-c", "source ../bootstrap.sh; "+fn)
}

// The tests load bootstrap.sh to call single functions. Loading it must
// not start the bring-up: that would talk to whatever lab the developer
// has running.
func TestBootstrapCanBeSourcedWithoutRunning(t *testing.T) {
	t.Parallel()
	run := runBootstrap(t, nil, "-c", "source ../bootstrap.sh")

	if run.code != 0 {
		t.Fatalf("sourcing bootstrap.sh exited %d: %s", run.code, run.stderr)
	}
	if len(run.calls) != 0 {
		t.Fatalf("sourcing bootstrap.sh touched the lab: %q", run.calls)
	}
}

// make e2e-up executes bootstrap.sh, so executing it still runs the
// bring-up. The stubbed lab never registers a chassis, so only the start
// of the bring-up is checked, not its exit code.
func TestBootstrapRunsMainWhenExecuted(t *testing.T) {
	t.Parallel()
	run := runBootstrap(t, nil, "../bootstrap.sh")

	if !strings.Contains(run.stderr, "[bootstrap] waiting for OVN NB at clab-ovn-e2e-central") {
		t.Fatalf("executing bootstrap.sh did not start the bring-up: %s", run.stderr)
	}
	if len(run.calls) == 0 {
		t.Fatal("executing bootstrap.sh made no docker call")
	}
}

// The docker calls the zebra gate makes on the upstream: one start
// attempt with the default readiness wait, the SIGKILL between attempts,
// and the first command of the FRR state dump.
const (
	zebraStartCall   = "exec -i --env ZEBRA_WAIT_SECS=30 clab-ovn-e2e-upstream sh -eu"
	zebraKillCall    = "exec clab-ovn-e2e-upstream pkill -KILL -f ^/usr/lib/frr/zebra"
	upstreamDumpCall = "exec clab-ovn-e2e-upstream cat /etc/frr/daemons"
)

// On a healthy lab watchfrr has started zebra before bring-up reaches
// the gate, so the gate passes on its first attempt and kills nothing.
// The script it sends finds zebra and bgpd by their full path, because
// the image's BusyBox pgrep and pkill never match the bare name, and
// starts zebra exactly as the image's own start path does.
func TestBootstrapZebraGatePassesOnARunningZebra(t *testing.T) {
	t.Parallel()
	run := runBootstrapFunc(t, "ensure_upstream_zebra", "START_EXITS=0")

	if run.code != 0 {
		t.Fatalf("the gate failed on a running zebra with exit %d: %s", run.code, run.stderr)
	}
	if got, want := run.startCalls(), []string{zebraStartCall}; !slices.Equal(got, want) {
		t.Fatalf("start calls = %q, want %q", got, want)
	}
	for _, call := range run.calls {
		if strings.Contains(call, "pkill") {
			t.Fatalf("the gate killed something on a healthy lab: %q", call)
		}
	}
	if !strings.Contains(run.stderr, "[bootstrap] zebra up on clab-ovn-e2e-upstream (attempt 1/3)") {
		t.Fatalf("the gate did not log the first attempt as the one that passed: %s", run.stderr)
	}
	for _, want := range []string{
		"/usr/lib/frr/zebra -d -F traditional -A 127.0.0.1 -s 90000000",
		"pgrep -f '^/usr/lib/frr/zebra'",
		"pkill -f '^/usr/lib/frr/bgpd'",
		"grep -qw zebra",
	} {
		if !strings.Contains(run.stdin, want) {
			t.Fatalf("the start script lacks %q:\n%s", want, run.stdin)
		}
	}
	for _, trap := range []string{"pgrep -x", "pkill -x"} {
		if strings.Contains(run.stdin, trap) {
			t.Fatalf("the start script uses %q, which BusyBox never matches:\n%s", trap, run.stdin)
		}
	}
}

// A zebra that crashes on start is started again. Each failed attempt is
// logged and its leftover zebra killed, and the bring-up goes on once an
// attempt passes, without the state dump of a failed bring-up. After a
// crash the kill finds no zebra and fails, which does not stop the retry.
func TestBootstrapZebraGateHealsOnALaterAttempt(t *testing.T) {
	t.Parallel()
	run := runBootstrapFunc(t, "ensure_upstream_zebra", "START_EXITS=1 1 0", "PKILL_EXIT=1")

	if run.code != 0 {
		t.Fatalf("the gate failed although the third attempt passed, exit %d: %s", run.code, run.stderr)
	}
	if got := len(run.startCalls()); got != 3 {
		t.Fatalf("made %d start attempts, want 3: %q", got, run.calls)
	}
	if got := run.count(zebraKillCall); got != 2 {
		t.Fatalf("killed zebra %d times, want once after each of the 2 failed attempts: %q", got, run.calls)
	}
	for _, want := range []string{
		"zebra start attempt 1/3 failed",
		"zebra start attempt 2/3 failed",
		"zebra up on clab-ovn-e2e-upstream (attempt 3/3)",
	} {
		if !strings.Contains(run.stderr, want) {
			t.Fatalf("stderr lacks %q: %s", want, run.stderr)
		}
	}
	if slices.Contains(run.calls, upstreamDumpCall) {
		t.Fatalf("a healed gate dumped the FRR state: %q", run.calls)
	}
}

// When no attempt brings zebra up, the bring-up stops at the gate with
// the FRR state dump and a last line that names zebra, whether zebra
// crashes, hangs without registering, or docker cannot reach the
// container. The cause each attempt reported stays in the job log above
// it. zebra is killed only between attempts: the last attempt's zebra is
// left for the dump, so a hung zebra still shows up in it.
func TestBootstrapZebraGateAbortsWhenTheBudgetIsUsedUp(t *testing.T) {
	t.Parallel()
	const want = "zebra did not come up on clab-ovn-e2e-upstream after 3 attempts; " +
		"without zebra the upstream installs no BGP route and no client reaches a floating IP"
	for _, tc := range []struct {
		name        string
		startStderr string
		pkillExit   string
	}{
		{
			name:        "zebra crashes in startup",
			startStderr: "zebra start exited 1: zebra crashed in startup, signal 11",
			pkillExit:   "1",
		},
		{
			name:        "zebra hangs without registering",
			startStderr: "zebra did not come up on clab-ovn-e2e-upstream within 30s",
			pkillExit:   "0",
		},
		{
			name:        "the upstream container is gone",
			startStderr: "Error response from daemon: No such container: clab-ovn-e2e-upstream",
			pkillExit:   "1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runBootstrapFunc(t, "ensure_upstream_zebra",
				"START_EXITS=1 1 1", "PKILL_EXIT="+tc.pkillExit, "START_STDERR="+tc.startStderr)

			if run.code != 1 {
				t.Fatalf("exit %d, want 1: %s", run.code, run.stderr)
			}
			if got := len(run.startCalls()); got != 3 {
				t.Fatalf("made %d start attempts, want 3: %q", got, run.calls)
			}
			if got := run.count(zebraKillCall); got != 2 {
				t.Fatalf("killed zebra %d times, want 2, only between the 3 attempts: %q", got, run.calls)
			}
			if !slices.Contains(run.calls, upstreamDumpCall) {
				t.Fatalf("the abort did not dump the FRR state: %q", run.calls)
			}
			if !strings.Contains(run.stderr, tc.startStderr) {
				t.Fatalf("the attempts' own error %q is missing: %s", tc.startStderr, run.stderr)
			}
			if got := run.lastLine(); got != want {
				t.Fatalf("last line = %q, want %q", got, want)
			}
		})
	}
}

// A budget that allows no attempt aborts the bring-up instead of letting
// it go on without a zebra anyone has seen.
func TestBootstrapZebraGateAbortsWithoutAnAttemptOnAnUnusableBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		attempts string
	}{
		{name: "zero attempts", attempts: "0"},
		{name: "a non-numeric budget", attempts: "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runBootstrapFunc(t, "ensure_upstream_zebra",
				"ZEBRA_START_ATTEMPTS="+tc.attempts, "START_EXITS=0")

			if run.code != 1 {
				t.Fatalf("exit %d, want 1: %s", run.code, run.stderr)
			}
			if starts := run.startCalls(); len(starts) != 0 {
				t.Fatalf("a budget of %q still started zebra: %q", tc.attempts, starts)
			}
			if !slices.Contains(run.calls, upstreamDumpCall) {
				t.Fatalf("the abort did not dump the FRR state: %q", run.calls)
			}
			want := "zebra did not come up on clab-ovn-e2e-upstream after " + tc.attempts + " attempts; " +
				"without zebra the upstream installs no BGP route and no client reaches a floating IP"
			if got := run.lastLine(); got != want {
				t.Fatalf("last line = %q, want %q", got, want)
			}
		})
	}
}

// An empty budget variable means its default, and a set readiness wait
// reaches the container.
func TestBootstrapZebraGateFallsBackToItsDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		env        []string
		wantStarts []string
	}{
		{
			name:       "empty variables",
			env:        []string{"ZEBRA_START_ATTEMPTS=", "ZEBRA_WAIT_SECS=", "START_EXITS=1 1 1"},
			wantStarts: []string{zebraStartCall, zebraStartCall, zebraStartCall},
		},
		{
			name:       "a set wait",
			env:        []string{"ZEBRA_WAIT_SECS=5", "START_EXITS=0"},
			wantStarts: []string{"exec -i --env ZEBRA_WAIT_SECS=5 clab-ovn-e2e-upstream sh -eu"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runBootstrapFunc(t, "ensure_upstream_zebra", tc.env...)

			if got := run.startCalls(); !slices.Equal(got, tc.wantStarts) {
				t.Fatalf("start calls = %q, want %q", got, tc.wantStarts)
			}
		})
	}
}

// bootstrapZebraScriptStubs stand in for the upstream container's tools
// while the zebra start script runs in a real sh. ZEBRA_RUNNING is a file
// that exists while a zebra process does. START_EXIT is the exit code of
// the zebra start, and a non-empty RUNS_AFTER makes the start leave a
// zebra process behind. A non-empty REGISTERS makes vtysh list zebra.
// PKILL_CALLS records every pkill.
var bootstrapZebraScriptStubs = map[string]string{
	"pgrep": `#!/bin/sh
# pgrep -f ^/usr/lib/frr/zebra: finds the zebra process or not
test -e "${ZEBRA_RUNNING}"
`,
	"pkill": `#!/bin/sh
# pkill: record argv; no bgpd runs, and BusyBox exits 1 when nothing matched
printf '%s\n' "$*" >>"${PKILL_CALLS}"
exit 1
`,
	"zebra": `#!/bin/sh
# zebra -d: leave a zebra process behind or not, then exit as scripted
[ -z "${RUNS_AFTER}" ] || : >"${ZEBRA_RUNNING}"
[ "${START_EXIT}" -eq 0 ] || echo "zebra start output" >&2
exit "${START_EXIT}"
`,
	"vtysh": `#!/bin/sh
# vtysh -c "show daemons"
if [ -n "${REGISTERS}" ]; then echo " zebra staticd"; else echo " staticd"; fi
`,
	"seq": `#!/bin/sh
# seq 1 LAST as BusyBox counts: nothing when LAST is below 1, an error
# when LAST is no number
case "$2" in
''|*[!0-9]*) echo "seq: invalid number '$2'" >&2; exit 1 ;;
esac
i=$1
while [ "${i}" -le "$2" ]; do echo "${i}"; i=$((i + 1)); done
`,
	"sleep":    bootstrapSleepStub,
	"hostname": "#!/bin/sh\necho upstream\n",
}

// The script start_upstream_zebra feeds to the container runs here in a
// real sh against stubs for the container's tools, so its exit is the
// script's own verdict on the zebra process it finds, not a scripted one.
// The seq stub counts as BusyBox does: nothing for `seq 1 0`, where the
// BSD seq of macOS counts down.
func TestBootstrapZebraStartScriptDecidesOnTheProcess(t *testing.T) {
	t.Parallel()
	const startCmd = "/usr/lib/frr/zebra -d"
	script := runBootstrapFunc(t, "start_upstream_zebra", "START_EXITS=0").stdin
	if !strings.Contains(script, startCmd) {
		t.Fatalf("the start script does not run %q:\n%s", startCmd, script)
	}
	script = strings.ReplaceAll(script, startCmd, "zebra -d")
	stubs := t.TempDir()
	for name, body := range bootstrapZebraScriptStubs {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s stub: %v", name, err)
		}
	}

	for _, tc := range []struct {
		name         string
		runsBefore   bool   // a zebra process exists when the script starts
		startExit    int    // exit code of the zebra start
		runsAfter    bool   // the start leaves a zebra process behind
		registers    bool   // vtysh show daemons lists zebra
		wait         string // ZEBRA_WAIT_SECS
		wantExit     int
		wantBgpdKill bool
		wantStderr   string
	}{
		{
			name:       "zebra already runs",
			runsBefore: true, registers: true, wait: "30",
		},
		{
			name:      "the start crashes and leaves no zebra",
			startExit: 139, wait: "30",
			wantExit: 1, wantBgpdKill: true,
			wantStderr: "zebra start exited 139: zebra start output\n",
		},
		{
			name:      "the start loses the race to watchfrr's zebra",
			startExit: 1, runsAfter: true, registers: true, wait: "30",
			wantBgpdKill: true,
			wantStderr:   "zebra start exited 1: zebra start output\n",
		},
		{
			name:      "watchfrr's zebra wins the race and never registers",
			startExit: 1, runsAfter: true, wait: "30",
			wantExit: 1, wantBgpdKill: true,
			wantStderr: "zebra start exited 1: zebra start output\n" +
				"zebra did not come up on upstream within 30s\n" +
				"last zebra start output: zebra start output\n",
		},
		{
			name:      "the started zebra never registers",
			runsAfter: true, wait: "30",
			wantExit: 1, wantBgpdKill: true,
			wantStderr: "zebra did not come up on upstream within 30s\n",
		},
		{
			name:       "a zero wait fails closed",
			runsBefore: true, registers: true, wait: "0",
			wantExit:   1,
			wantStderr: "zebra did not come up on upstream within 0s\n",
		},
		{
			name:       "a non-numeric wait fails closed",
			runsBefore: true, registers: true, wait: "abc",
			wantExit: 1,
			wantStderr: "seq: invalid number 'abc'\n" +
				"zebra did not come up on upstream within abcs\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := t.TempDir()
			running := filepath.Join(state, "zebra-running")
			kills := filepath.Join(state, "pkill-calls")
			if tc.runsBefore {
				if err := os.WriteFile(running, nil, 0o644); err != nil {
					t.Fatalf("mark zebra as running: %v", err)
				}
			}
			flag := func(on bool) string {
				if on {
					return "1"
				}
				return ""
			}
			sh := exec.Command("sh", "-eu")
			sh.Stdin = strings.NewReader(script)
			sh.Env = []string{
				"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
				"ZEBRA_WAIT_SECS=" + tc.wait,
				"ZEBRA_RUNNING=" + running,
				"PKILL_CALLS=" + kills,
				"START_EXIT=" + strconv.Itoa(tc.startExit),
				"RUNS_AFTER=" + flag(tc.runsAfter),
				"REGISTERS=" + flag(tc.registers),
			}
			var stderr bytes.Buffer
			sh.Stderr = &stderr

			code := 0
			if err := sh.Run(); err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("run the start script: %v", err)
				}
				code = exitErr.ExitCode()
			}

			if code != tc.wantExit {
				t.Fatalf("exit %d, want %d: %s", code, tc.wantExit, stderr.String())
			}
			if got := stderr.String(); got != tc.wantStderr {
				t.Fatalf("stderr = %q, want %q", got, tc.wantStderr)
			}
			calls, err := os.ReadFile(kills)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("read the recorded pkill calls: %v", err)
			}
			if got := strings.Contains(string(calls), "-f ^/usr/lib/frr/bgpd"); got != tc.wantBgpdKill {
				t.Fatalf("killed bgpd = %t, want %t: %q", got, tc.wantBgpdKill, calls)
			}
		})
	}
}

// The end check passes a lab whose zebra still runs after one look, and
// never starts anything.
func TestBootstrapEndCheckPassesWhileZebraRuns(t *testing.T) {
	t.Parallel()
	run := runBootstrapFunc(t, "verify_upstream_zebra", "PGREP_EXIT=0")

	if run.code != 0 {
		t.Fatalf("the end check failed on a running zebra with exit %d: %s", run.code, run.stderr)
	}
	if !strings.Contains(run.stderr, "[bootstrap] zebra still running on clab-ovn-e2e-upstream") {
		t.Fatalf("the end check did not log its look: %s", run.stderr)
	}
	if len(run.calls) != 1 {
		t.Fatalf("made %d docker calls, want the one look: %q", len(run.calls), run.calls)
	}
	if starts := run.startCalls(); len(starts) != 0 {
		t.Fatalf("the end check started zebra: %q", starts)
	}
}

// A zebra that died after it came up ends the bring-up with the FRR state
// dump and a line that names zebra. The end check does not start zebra
// again, and a docker exec that fails for any other reason counts as a
// dead zebra.
func TestBootstrapEndCheckAbortsWhenZebraIsGone(t *testing.T) {
	t.Parallel()
	const want = "zebra on clab-ovn-e2e-upstream died after it came up; " +
		"without zebra the upstream installs no BGP route and no client reaches a floating IP"
	for _, tc := range []struct {
		name      string
		pgrepExit string
	}{
		{name: "zebra is gone", pgrepExit: "1"},
		{name: "docker exec fails", pgrepExit: "125"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runBootstrapFunc(t, "verify_upstream_zebra", "PGREP_EXIT="+tc.pgrepExit)

			if run.code != 1 {
				t.Fatalf("exit %d, want 1: %s", run.code, run.stderr)
			}
			if starts := run.startCalls(); len(starts) != 0 {
				t.Fatalf("the end check started zebra: %q", starts)
			}
			if !slices.Contains(run.calls, upstreamDumpCall) {
				t.Fatalf("the abort did not dump the FRR state: %q", run.calls)
			}
			if got := run.lastLine(); got != want {
				t.Fatalf("last line = %q, want %q", got, want)
			}
		})
	}
}

// bgpd may only start once zebra is up, and the end check has to see the
// finished bring-up, so main runs the gate right before the upstream FRR
// setup and the end check right before it reports success. Every function
// but main and log is replaced by one that only logs its name, so the
// steps are the ones main runs, in the order it runs them.
func TestBootstrapMainGatesZebraAroundTheUpstreamFRRSetup(t *testing.T) {
	t.Parallel()
	run := runBootstrap(t, nil, "-c", `source ../bootstrap.sh
for f in $(compgen -A function); do
    case "${f}" in main|log) ;; *) eval "${f}() { log \"step ${f}\"; }" ;; esac
done
main`)

	if run.code != 0 {
		t.Fatalf("main with every step stubbed exited %d: %s", run.code, run.stderr)
	}
	var steps []string
	for _, line := range strings.Split(run.stderr, "\n") {
		if step, ok := strings.CutPrefix(line, "[bootstrap] step "); ok {
			steps = append(steps, step)
		}
	}
	gate := slices.Index(steps, "ensure_upstream_zebra")
	if gate < 0 || gate == len(steps)-1 || steps[gate+1] != "configure_upstream_frr" {
		t.Fatalf("main runs %q, want ensure_upstream_zebra directly before configure_upstream_frr", steps)
	}
	if !slices.Contains(steps, "converge_master_chassis") || steps[len(steps)-1] != "verify_upstream_zebra" {
		t.Fatalf("main runs %q, want verify_upstream_zebra last, after converge_master_chassis", steps)
	}
	if got, want := run.lastLine(), "[bootstrap] bootstrap complete"; got != want {
		t.Fatalf("last line = %q, want %q right after verify_upstream_zebra", got, want)
	}
}
