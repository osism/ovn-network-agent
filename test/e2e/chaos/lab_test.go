package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseOVSDBTableFlattensUUIDsAndSets(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    map[string][]string
		wantErr bool
	}{
		{
			name: "a single-chassis claim",
			out: `{"headings":["logical_port","chassis","additional_chassis"],
			       "data":[["cr-lr0-public",["uuid","abc"],["set",[]]]]}`,
			want: map[string][]string{
				"logical_port":       {"cr-lr0-public"},
				"chassis":            {"abc"},
				"additional_chassis": nil,
			},
		},
		{
			name: "an unbound port during re-election",
			out: `{"headings":["logical_port","chassis","additional_chassis"],
			       "data":[["cr-lr0-public",["set",[]],["set",[]]]]}`,
			want: map[string][]string{
				"logical_port":       {"cr-lr0-public"},
				"chassis":            nil,
				"additional_chassis": nil,
			},
		},
		{
			name: "two chassis on one port",
			out: `{"headings":["logical_port","chassis","additional_chassis"],
			       "data":[["cr-lr0-public",["uuid","abc"],["set",[["uuid","def"],["uuid","ghi"]]]]]}`,
			want: map[string][]string{
				"logical_port":       {"cr-lr0-public"},
				"chassis":            {"abc"},
				"additional_chassis": {"def", "ghi"},
			},
		},
		{
			name:    "not JSON at all",
			out:     "ovn-sbctl: unix:/var/run/ovn/ovnsb_db.sock: database connection failed",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rows, err := parseOVSDBTable(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOVSDBTable(%q) = %v, want an error", tc.out, rows)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOVSDBTable: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("parsed %d rows, want 1", len(rows))
			}
			for col, want := range tc.want {
				got := rows[0][col]
				if len(got) != len(want) {
					t.Fatalf("column %s = %v, want %v", col, got, want)
				}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("column %s = %v, want %v", col, got, want)
					}
				}
			}
		})
	}
}

// The dual-claim check must see both the `chassis` column and every
// member of `additional_chassis` — a port claimed by two chassis at once
// is the split the run must never observe.
func TestCRPortClaimsCollectsEveryClaimingChassis(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) {
		return `{"headings":["logical_port","chassis","additional_chassis"],
		         "data":[["cr-lr0-public",["uuid","abc"],["set",[["uuid","def"]]]]]}`, nil
	}}
	l := newTestLab(cmd, newFakeClock())

	claims, err := l.crPortClaims(context.Background())
	if err != nil {
		t.Fatalf("crPortClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("got %d claims, want 1", len(claims))
	}
	if claims[0].port != crPort {
		t.Fatalf("port = %q, want %q", claims[0].port, crPort)
	}
	if len(claims[0].chassis) != 2 {
		t.Fatalf("chassis = %v, want the chassis column plus the additional one", claims[0].chassis)
	}
}

func TestCRPortClaimsReportsALookupFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	if _, err := l.crPortClaims(context.Background()); err == nil {
		t.Fatal("a failed sbctl lookup was reported as no claims at all")
	}
}

// currentMaster resolves the chassis UUID on the Port_Binding to a name.
// An unbound port (re-election in flight) is an empty name, not an error
// — the caller polls until it settles.
func TestCurrentMaster(t *testing.T) {
	tests := []struct {
		name string
		uuid string
		want string
	}{
		{name: "bound to a chassis", uuid: "abc\n", want: "gateway-2"},
		{name: "unbound during re-election", uuid: "\n", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &fakeCommander{respond: func(argv []string) (string, error) {
				line := strings.Join(argv, " ")
				if strings.Contains(line, "--columns=chassis find Port_Binding") {
					return tc.uuid, nil
				}
				return "gateway-2\n", nil
			}}
			l := newTestLab(cmd, newFakeClock())

			if got := l.currentMaster(context.Background()); got != tc.want {
				t.Fatalf("currentMaster = %q, want %q", got, tc.want)
			}
		})
	}
}

// The VIP's two hand-plumbed routes follow the master: the forward route
// on `upstream` points at the new master's underlay IP, and the
// scope-link route is installed on the new master itself.
func TestEnsureVIPRoutingFollowsTheMaster(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	if err := l.ensureVIPRouting(context.Background(), "gateway-2"); err != nil {
		t.Fatalf("ensureVIPRouting: %v", err)
	}

	want := []string{
		"docker exec clab-ovn-e2e-upstream ip route replace 192.0.2.50/32 via 100.64.2.2",
		"docker exec clab-ovn-e2e-gateway-2 ip route replace 192.0.2.50/32 dev br-ex scope link",
	}
	got := cmd.lines()
	if len(got) != len(want) {
		t.Fatalf("issued %d commands, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEnsureVIPRoutingRejectsAnUnknownMaster(t *testing.T) {
	l := newTestLab(&fakeCommander{}, newFakeClock())

	if err := l.ensureVIPRouting(context.Background(), "gateway-9"); err == nil {
		t.Fatal("a master with no underlay link was accepted")
	}
}

// Every lab primitive must wrap a failed command with what it was
// trying to do and on which node — a bare exec error in the journal
// names neither.
func TestLabPrimitivesWrapFailuresWithContext(t *testing.T) {
	tests := []struct {
		name string
		call func(*lab) error
		want string
	}{
		{"stop the controller", func(l *lab) error { return l.stopController(ctxT(), "gateway-1") }, "stop ovn-controller on gateway-1"},
		{"start the controller", func(l *lab) error { return l.startController(ctxT(), "gateway-1") }, "start ovn-controller on gateway-1"},
		{"kill the container", func(l *lab) error { return l.killGateway(ctxT(), "gateway-2") }, "kill gateway-2"},
		{"start the container", func(l *lab) error { return l.startGateway(ctxT(), "gateway-2") }, "start gateway-2"},
		{"restart the container", func(l *lab) error { return l.restartGateway(ctxT(), "gateway-3") }, "restart gateway-3"},
		{"signal the agent", func(l *lab) error { return l.terminateAgent(ctxT(), "gateway-3") }, "terminate agent on gateway-3"},
		{"set the restart policy", func(l *lab) error { return l.setRestartPolicy(ctxT(), "gateway-1", "no") }, `set restart policy "no" on gateway-1`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}

			err := tc.call(newTestLab(cmd, newFakeClock()))

			if err == nil {
				t.Fatalf("%s: a failed command was reported as success", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
			if !errors.Is(err, errBoom) {
				t.Fatalf("error %q dropped the cause from the chain", err)
			}
		})
	}
}

// A daemon that never comes back must fail the re-wire, not let it push
// config at a container that cannot accept it.
func TestWaitReadyGivesUpOnADaemonThatNeverComesBack(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	err := l.waitReady(context.Background(), "gateway-1", "ovs-vsctl show")
	if err == nil {
		t.Fatal("waitReady returned success for a daemon that never answered")
	}
	if !strings.Contains(err.Error(), "ovs-vsctl show") || !strings.Contains(err.Error(), "gateway-1") {
		t.Fatalf("error %q names neither the probe nor the node", err)
	}
}

// docker inspect fails on a container that is gone; the run must read
// that as "not healthy", never as healthy.
func TestContainerHealthOfAMissingContainerIsNotHealthy(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	if got := l.containerHealth(context.Background(), "gateway-1"); got == "healthy" {
		t.Fatalf("containerHealth = %q for a container docker cannot inspect", got)
	}
}

// The convergence gate only lets a node back into service once its
// chassis is registered in SB again. A lookup that could not be answered
// is not a registration.
func TestChassisInSB(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{name: "the chassis is registered", out: "gateway-2\n", want: true},
		{name: "the chassis has not come back", out: "\n", want: false},
		{name: "the lookup failed", err: errBoom, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &fakeCommander{respond: func([]string) (string, error) { return tc.out, tc.err }}
			l := newTestLab(cmd, newFakeClock())

			if got := l.chassisInSB(context.Background(), "gateway-2"); got != tc.want {
				t.Fatalf("chassisInSB = %v, want %v", got, tc.want)
			}
		})
	}
}

// docker inspect can fail on a busy daemon. Reading that as an exit would
// report a container that never stopped as a clean termination — and the
// restore would then `docker start` a node that is still running, fail,
// and record a fabricated violation against a lab that was fine.
func TestWaitContainerExitDoesNotReadAFailedInspectAsAnExit(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	err := l.waitContainerExit(context.Background(), "gateway-3", 5*time.Second)

	if err == nil {
		t.Fatal("a container docker could not inspect was reported as exited")
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("error %q dropped the inspect failure from the chain", err)
	}
}

// A dual-claim violation names the chassis. When the SB row is already
// gone the UUID is better than nothing — but it must not be silently
// dropped.
func TestChassisNameFallsBackToTheUUID(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	if got := l.chassisName(context.Background(), "abc-uuid"); got != "abc-uuid" {
		t.Fatalf("chassisName = %q, want the UUID back", got)
	}
}

// A `docker exec` against a lab the runner is actively breaking can hang
// for good, and no context reaching the commander carries a deadline of
// its own. An invocation that never returns wedges its caller: a prober
// frozen mid-sample keeps reporting the value it last saw, so a data path
// that is dead reads as green and the engine declares the node converged.
func TestExecCommanderBoundsAWedgedCommand(t *testing.T) {
	cmd := execCommander{timeout: 50 * time.Millisecond}

	start := time.Now()
	_, err := cmd.run(context.Background(), "sleep", "10")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command that never returned on its own was reported as success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the command ran for %s past its 50ms budget", elapsed)
	}
}

func ctxT() context.Context { return context.Background() }

// pgrep exits 1 when nothing matches — that is the condition under test,
// and an answer. Every other failure means the question could not be
// asked: a docker daemon under load, an expired cmdTimeout. Collapsing
// those into "no agent" turns a busy runner into a false agent-down
// violation and fails the run over a lab that was fine.
func TestAgentAlive(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		err     func(*testing.T) error
		want    bool
		wantErr bool
	}{
		{
			name: "the agent is running",
			out:  "1\n",
			want: true,
		},
		{
			name: "pgrep matched nothing",
			err:  func(t *testing.T) error { return errExit(t, 1) },
			want: false,
		},
		{
			name:    "the probe could not be answered at all",
			err:     func(t *testing.T) error { return errBoom },
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &fakeCommander{respond: func([]string) (string, error) {
				if tc.err != nil {
					return "", tc.err(t)
				}
				return tc.out, nil
			}}
			l := newTestLab(cmd, newFakeClock())

			got, err := l.agentAlive(context.Background(), "gateway-1")

			if tc.wantErr {
				if err == nil {
					t.Fatal("a probe the runner could not answer was reported as a dead agent")
				}
				return
			}
			if err != nil {
				t.Fatalf("agentAlive: %v", err)
			}
			if got != tc.want {
				t.Fatalf("agentAlive = %v, want %v", got, tc.want)
			}
			if !cmd.called("pgrep -f /usr/local/bin/ovn-network-agent") {
				t.Fatalf("agentAlive did not pgrep for the agent: %v", cmd.lines())
			}
		})
	}
}

// Every caller parses the command's output — OVSDB JSON, docker -f
// templates, chassis names. ovn-sbctl logs its ovsdb-idl reconnect and
// transaction warnings to stderr while still exiting 0, which is exactly
// what a chassis the runner has just SIGKILLed provokes. Merged into
// stdout, one WARN line is a JSON parse error or a corrupted UUID.
func TestExecCommanderKeepsStderrOutOfTheOutput(t *testing.T) {
	cmd := execCommander{}

	out, err := cmd.run(context.Background(), "sh", "-c",
		`echo "ovsdb-idl|WARN|reconnecting" >&2; printf '%s' '{"headings":[]}'`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != `{"headings":[]}` {
		t.Fatalf("output = %q, want stdout alone", out)
	}

	// A failure still has to say what the command complained about.
	_, err = cmd.run(context.Background(), "sh", "-c", "echo 'no such container' >&2; exit 1")
	if err == nil || !strings.Contains(err.Error(), "no such container") {
		t.Fatalf("error = %v, want one carrying the command's stderr", err)
	}
}

// A restore chains four of these wait loops inside one bounded context.
// One that never looks at the context keeps spinning out its own 60s
// clock against commands that the dead context fails instantly — so the
// restore overruns the deadline that was supposed to bound it, and the
// run records the runner's own arithmetic as a failed action.
func TestWaitReadyGivesUpOnADeadContext(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	clock := newFakeClock()
	l := newTestLab(cmd, clock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := clock.now()
	err := l.waitReady(ctx, "gateway-1", "ovs-vsctl show")

	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady = %v, want the context's own error", err)
	}
	if clock.now() != started {
		t.Fatalf("waitReady kept polling for %s on a context that was already done",
			clock.now().Sub(started))
	}
	if len(cmd.lines()) != 0 {
		t.Fatalf("waitReady probed the node on a dead context: %v", cmd.lines())
	}
}
