package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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
// behind: its exit code, its stderr and the docker calls it made.
type bootstrapRun struct {
	code   int
	stderr string
	calls  []string
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
	script := exec.Command("bash", bashArgs...)
	script.Env = append([]string{
		"PATH=" + stubs + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DOCKER_CALLS=" + callsFile,
		"DOCKER_STDIN=" + filepath.Join(stubs, "docker-stdin"),
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
	calls, err := os.ReadFile(callsFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read the recorded docker calls: %v", err)
	}
	if lines := strings.TrimSuffix(string(calls), "\n"); lines != "" {
		run.calls = strings.Split(lines, "\n")
	}
	return run
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
