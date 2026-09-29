package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func actionNamed(t *testing.T, name string) *action {
	t.Helper()
	for _, a := range starterActions(defaultTestProfile(t)) {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no action named %q in the registry", name)
	return nil
}

// The registry order and the weights are part of the replay contract: a
// new action is appended, never inserted, or every recorded seed replays
// a different sequence.
func TestStarterRegistryOrderIsStable(t *testing.T) {
	want := []string{"controller-restart", "gateway-kill", "agent-terminate", "gateway-restart"}

	var got []string
	for _, a := range starterActions(defaultTestProfile(t)) {
		got = append(got, a.name)
		if a.weight <= 0 {
			t.Fatalf("action %s ships with weight %d", a.name, a.weight)
		}
		if a.holdMax < a.holdMin {
			t.Fatalf("action %s has holdMax %s below holdMin %s", a.name, a.holdMax, a.holdMin)
		}
		if a.recoveryBudget <= 0 {
			t.Fatalf("action %s has no recovery budget", a.name)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("registry = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry = %v, want %v", got, want)
		}
	}
}

// containerlab deploys the gateways with `restart: always`. Killing one
// without disabling the policy first means docker revives it before the
// fault is observable at all — the kill would measure nothing.
func TestGatewayKillDisablesTheRestartPolicyBeforeTheKill(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	if err := actionNamed(t, "gateway-kill").inject(context.Background(), l, "gateway-2", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	disable := cmd.indexOf("update --restart=no clab-ovn-e2e-gateway-2")
	kill := cmd.indexOf("kill -s KILL clab-ovn-e2e-gateway-2")
	if disable < 0 || kill < 0 {
		t.Fatalf("kill did not disable the restart policy and SIGKILL the node: %v", cmd.lines())
	}
	if disable > kill {
		t.Fatalf("the restart policy was disabled after the kill: %v", cmd.lines())
	}
}

// Restoring a killed node has to put back what `docker start` does not:
// the containerlab veth to `upstream`, the underlay address, the BGP
// session, and the restart policy containerlab set.
func TestGatewayKillRestoreRewiresTheUnderlay(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := actionNamed(t, "gateway-kill").restore(context.Background(), l, "gateway-2"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	for _, want := range []string{
		"docker start clab-ovn-e2e-gateway-2",
		"docker update --restart=always clab-ovn-e2e-gateway-2",
		"containerlab tools veth create -a clab-ovn-e2e-gateway-2:eth1 -b clab-ovn-e2e-upstream:eth2",
		"ip addr replace 100.64.2.2/30 dev eth1",
		"ip addr replace 100.64.2.1/30 dev eth2",
		"router bgp 65000 vrf vrf-provider",
	} {
		if !cmd.called(want) {
			t.Fatalf("restore did not issue %q: %v", want, cmd.lines())
		}
	}
	start := cmd.indexOf("docker start clab-ovn-e2e-gateway-2")
	rewire := cmd.indexOf("containerlab tools veth create")
	if start > rewire {
		t.Fatalf("the veth was re-created before the container was started: %v", cmd.lines())
	}
}

// The veth survives an ovn-controller restart, so it must not be
// re-created when it is still there — `veth create` would fail on an
// existing interface.
func TestRewireSkipsTheVethWhenTheInterfaceSurvived(t *testing.T) {
	// Unlike healthyLabResponses, this lab still has its eth1, so the
	// `ip link show eth1` probe succeeds and the veth is not re-created. Its
	// address and BGP config are answered by healthyLabResponses, which the
	// rewire's own verification reads back.
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "ip link show eth1") {
			return "", nil
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	if err := l.rewireUnderlay(context.Background(), "gateway-1"); err != nil {
		t.Fatalf("rewireUnderlay: %v", err)
	}

	if cmd.called("containerlab tools veth create") {
		t.Fatalf("re-created a veth that was still present: %v", cmd.lines())
	}
}

// errCollision is what containerlab 0.77.0 reports when `veth create` cannot
// rename the new pair's upstream end because the previous incarnation's end
// still holds the name, folded into the error the way execCommander folds
// stderr.
var errCollision = errors.New("containerlab tools veth create ...: exit status 1: " +
	"ERROR Failed to deploy veth endpoint: failed to rename link: file exists")

// upstreamEndTornDownAfter answers the upstream `ip link show eth2` probe as
// present for its first n calls and as gone after, the way the kernel's
// asynchronous teardown of the previous incarnation's veth looks from
// upstream. Everything else is answered by healthyLabResponses.
func upstreamEndTornDownAfter(n int) func(argv []string) (string, error) {
	probes := 0
	return func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "clab-ovn-e2e-upstream ip link show eth2") {
			probes++
			if probes <= n {
				return "", nil
			}
		}
		return healthyLabResponses(argv)
	}
}

// A restore reaches `veth create` seconds after the old container exited,
// while the kernel may still be tearing down the old pair's upstream end.
// Creating then collides on the name, so the rewire waits for the end to go.
func TestRewireWaitsForTheStaleUpstreamEndBeforeRecreatingTheVeth(t *testing.T) {
	cmd := &fakeCommander{respond: upstreamEndTornDownAfter(3)}
	clock := newFakeClock()
	l := newTestLab(cmd, clock)
	var notes []string
	l.note = func(detail string) { notes = append(notes, detail) }

	started := clock.now()
	if err := l.rewireUnderlay(context.Background(), "gateway-2"); err != nil {
		t.Fatalf("rewireUnderlay: %v", err)
	}

	if got := cmd.count("containerlab tools veth create"); got != 1 {
		t.Fatalf("veth create ran %d times, want once: %v", got, cmd.lines())
	}
	fourthProbe, probes := -1, 0
	for i, line := range cmd.lines() {
		if strings.Contains(line, "clab-ovn-e2e-upstream ip link show eth2") {
			if probes++; probes == 4 {
				fourthProbe = i
			}
		}
	}
	if create := cmd.indexOf("containerlab tools veth create"); fourthProbe < 0 || create < fourthProbe {
		t.Fatalf("veth create ran before upstream:eth2 was seen gone: %v", cmd.lines())
	}
	if cmd.called("ip link del") {
		t.Fatalf("deleted an upstream end the kernel tore down within the budget: %v", cmd.lines())
	}
	if waited := clock.now().Sub(started); waited != 3*vethRecreatePollInterval {
		t.Fatalf("the rewire waited %s, want %s", waited, 3*vethRecreatePollInterval)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "upstream:eth2 seen stale: true") ||
		!strings.Contains(notes[0], "deleted: false") {
		t.Fatalf("notes = %q, want one saying upstream:eth2 was seen stale and not deleted", notes)
	}
}

// Every command a real restore issues takes time. A re-creation whose first
// create succeeds on a free name had nothing to work around, however long its
// probe and the create itself took, and must not be noted as if it had.
func TestRewireNotesNothingWhenTheFirstCreateSucceeds(t *testing.T) {
	clock := newFakeClock()
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		clock.sleep(500 * time.Millisecond)
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, clock)
	var notes []string
	l.note = func(detail string) { notes = append(notes, detail) }

	if err := l.rewireUnderlay(context.Background(), "gateway-2"); err != nil {
		t.Fatalf("rewireUnderlay: %v", err)
	}

	if !cmd.called("containerlab tools veth create") {
		t.Fatalf("the veth was not re-created: %v", cmd.lines())
	}
	if len(notes) != 0 {
		t.Fatalf("a re-creation that succeeded at the first try was noted: %q", notes)
	}
}

// An upstream end that outlives the budget can only be the dead peer of a
// netns that no longer exists: the gateway's own eth1 is already gone. It is
// deleted rather than failing the restore.
func TestRewireDeletesTheStaleUpstreamEndWhenItOutlivesTheBudget(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "clab-ovn-e2e-upstream ip link show eth2") {
			return "", nil
		}
		return healthyLabResponses(argv)
	}}
	clock := newFakeClock()
	l := newTestLab(cmd, clock)
	var notes []string
	l.note = func(detail string) { notes = append(notes, detail) }

	started := clock.now()
	if err := l.rewireUnderlay(context.Background(), "gateway-2"); err != nil {
		t.Fatalf("rewireUnderlay: %v", err)
	}

	del := cmd.indexOf("docker exec clab-ovn-e2e-upstream ip link del eth2")
	create := cmd.indexOf("containerlab tools veth create")
	if del < 0 || del > create {
		t.Fatalf("the stale upstream end was not deleted before the veth create: %v", cmd.lines())
	}
	if got := cmd.count("containerlab tools veth create"); got != 1 {
		t.Fatalf("veth create ran %d times, want once: %v", got, cmd.lines())
	}
	if waited := clock.now().Sub(started); waited < vethRecreateTimeout {
		t.Fatalf("the stale end was deleted after %s, before the %s budget ran out",
			waited, vethRecreateTimeout)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "deleted: true") {
		t.Fatalf("notes = %q, want one saying deleted: true", notes)
	}
}

// When the rename of the upstream end collides, containerlab 0.77.0 leaves
// the new pair behind with its gateway end already named eth1. The rewire
// removes it before trying again, or the retry would collide on the gateway.
func TestRewireRetriesVethCreateOnAFileExistsCollision(t *testing.T) {
	creates := 0
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "containerlab tools veth create") {
			if creates++; creates == 1 {
				return "", errCollision
			}
			return "", nil
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())
	var notes []string
	l.note = func(detail string) { notes = append(notes, detail) }

	if err := l.rewireUnderlay(context.Background(), "gateway-2"); err != nil {
		t.Fatalf("rewireUnderlay: %v", err)
	}

	if got := cmd.count("containerlab tools veth create"); got != 2 {
		t.Fatalf("veth create ran %d times, want the attempt and one retry: %v", got, cmd.lines())
	}
	var createAt []int
	for i, line := range cmd.lines() {
		if strings.Contains(line, "containerlab tools veth create") {
			createAt = append(createAt, i)
		}
	}
	del := cmd.indexOf("docker exec clab-ovn-e2e-gateway-2 ip link del eth1")
	if del <= createAt[0] || del >= createAt[1] {
		t.Fatalf("the half-created gateway end was not deleted between the two creates: %v", cmd.lines())
	}
	// The upstream end read as gone, so the note must not claim a wait for it.
	if len(notes) != 1 || !strings.Contains(notes[0], "seen stale: false") ||
		!strings.Contains(notes[0], "collided 1 time(s)") {
		t.Fatalf("notes = %q, want one saying the end was not seen stale and the create collided once", notes)
	}
}

// A collision that never clears must still end within the budget, and the
// error has to say which end was in the way.
func TestRewireFailsNamingTheUpstreamEndWhenVethCreateKeepsColliding(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "containerlab tools veth create") {
			return "", errCollision
		}
		return healthyLabResponses(argv)
	}}
	clock := newFakeClock()
	l := newTestLab(cmd, clock)
	var notes []string
	l.note = func(detail string) { notes = append(notes, detail) }

	started := clock.now()
	err := l.rewireUnderlay(context.Background(), "gateway-2")

	if err == nil {
		t.Fatal("a veth create that kept colliding was reported as a successful rewire")
	}
	if !errors.Is(err, errCollision) {
		t.Fatalf("error %q does not wrap the last create error", err)
	}
	for _, want := range []string{"re-create underlay veth for gateway-2", "upstream:eth2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if waited := clock.now().Sub(started); waited < vethRecreateTimeout {
		t.Fatalf("the rewire gave up after %s, before the %s budget ran out", waited, vethRecreateTimeout)
	} else if waited > vethRecreateTimeout+vethRecreatePollInterval {
		t.Fatalf("the rewire kept retrying for %s, past the %s budget", waited, vethRecreateTimeout)
	}
	if got := cmd.count("containerlab tools veth create"); got < 2 {
		t.Fatalf("veth create ran %d times, want it retried: %v", got, cmd.lines())
	}
	if len(notes) != 0 {
		t.Fatalf("a failed re-creation was noted as healed: %q", notes)
	}
}

// The restore's bounded context can expire while the rewire waits; the
// rewire must then stop at once instead of creating a veth nobody waits for.
func TestRewireGivesUpWhenTheContextExpiresWhileItWaits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// The context expires on the second look at the upstream end, while the
	// end is still there and the rewire is waiting for it.
	tornDown := upstreamEndTornDownAfter(10)
	probes := 0
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "clab-ovn-e2e-upstream ip link show eth2") {
			if probes++; probes == 2 {
				cancel()
			}
		}
		return tornDown(argv)
	}}
	clock := newFakeClock()
	l := newTestLab(cmd, clock)

	started := clock.now()
	err := l.rewireUnderlay(ctx, "gateway-2")

	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("rewireUnderlay = %v, want the context's own error", err)
	}
	if waited := clock.now().Sub(started); waited >= vethRecreateTimeout {
		t.Fatalf("the rewire waited out its %s budget on a context that had expired", vethRecreateTimeout)
	}
	if cmd.called("containerlab tools veth create") || cmd.called("ip link del") {
		t.Fatalf("acted on the veth after the context expired: %v", cmd.lines())
	}
}

// Outside the engine nobody listens for notes, and a re-creation that had
// something to report must not trip over the missing listener.
func TestRewireNoteIsOptional(t *testing.T) {
	cmd := &fakeCommander{respond: upstreamEndTornDownAfter(3)}
	l := newTestLab(cmd, newFakeClock())

	if err := l.rewireUnderlay(context.Background(), "gateway-2"); err != nil {
		t.Fatalf("rewireUnderlay: %v", err)
	}
	if got := cmd.count("containerlab tools veth create"); got != 1 {
		t.Fatalf("veth create ran %d times, want once: %v", got, cmd.lines())
	}
}

func TestRewireRejectsAnUnknownGateway(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	err := l.rewireUnderlay(context.Background(), "gateway-9")

	if err == nil || err.Error() != "no underlay link known for gateway-9" {
		t.Fatalf("rewireUnderlay = %v, want no underlay link known for gateway-9", err)
	}
	if len(cmd.lines()) != 0 {
		t.Fatalf("issued commands for a gateway with no underlay link: %v", cmd.lines())
	}
}

// Only a name collision is worth a retry. Any other failure, including an
// exec that could not run at all, is returned as it is.
func TestIsVethCollision(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"containerlab rename collision", errCollision, true},
		{"iproute2 spelling", errors.New("exit status 2: RTNETLINK answers: File exists"), true},
		{"a failure that is not a collision", errBoom, false},
		{"a non-zero exit without the message", errExit(t, 1), false},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVethCollision(tc.err); got != tc.want {
				t.Fatalf("isVethCollision(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// controller-restart is the soft fault: it never touches the container,
// so the containerlab veths stay up and no re-wire is needed.
func TestControllerRestartUsesOvnCtlAndTouchesNoContainer(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())
	act := actionNamed(t, "controller-restart")

	if err := act.inject(context.Background(), l, "gateway-1", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := act.restore(context.Background(), l, "gateway-1"); err != nil {
		t.Fatalf("restore: %v", err)
	}

	want := []string{
		"docker exec clab-ovn-e2e-gateway-1 /usr/share/ovn/scripts/ovn-ctl stop_controller",
		"docker exec clab-ovn-e2e-gateway-1 /usr/share/ovn/scripts/ovn-ctl start_controller",
	}
	got := cmd.lines()
	if len(got) != len(want) {
		t.Fatalf("issued %d commands, want exactly the two ovn-ctl calls: %v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The SIGTERM goes to the container's init — tini, which forwards it to
// the agent it supervises — and the container follows the agent down.
func TestAgentTerminateSignalsPID1AndWaitsForTheExit(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := actionNamed(t, "agent-terminate").inject(context.Background(), l, "gateway-3", 0); err != nil {
		t.Fatalf("inject: %v", err)
	}

	if !cmd.called("docker exec clab-ovn-e2e-gateway-3 kill -TERM 1") {
		t.Fatalf("the agent was not signalled through PID 1: %v", cmd.lines())
	}
	if !cmd.called("{{.State.Running}}") {
		t.Fatalf("the runner did not wait for the container to exit: %v", cmd.lines())
	}
}

// An agent that ignores its SIGTERM leaves the node in a state the run
// cannot reason about; the engine turns that error into a violation.
func TestAgentTerminateFailsWhenTheContainerStaysUp(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "{{.State.Running}}") {
			return "true\n", nil
		}
		return "", nil
	}}
	l := newTestLab(cmd, newFakeClock())

	err := actionNamed(t, "agent-terminate").inject(context.Background(), l, "gateway-3", 0)
	if err == nil {
		t.Fatal("a container that never exited was reported as a clean termination")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("error %q does not say the container stayed up", err)
	}
}

// The restart policy is pinned to "no" for the duration of the fault, so
// docker cannot revive the node behind the runner's back. A start that
// fails must still put it back — otherwise the lab keeps a gateway docker
// will never bring up again, long after the run is over.
func TestStartAndRestoreGatewayRestoresThePolicyWhenTheStartFails(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "docker start") {
			return "", errBoom
		}
		return healthyLabResponses(argv)
	}}
	l := newTestLab(cmd, newFakeClock())

	err := startAndRestoreGateway(context.Background(), l, defaultTestProfile(t), "gateway-2")

	if err == nil {
		t.Fatal("a container that could not be started was reported as restored")
	}
	if !cmd.called("update --restart=always clab-ovn-e2e-gateway-2") {
		t.Fatalf("the restart policy was left disabled after a failed start: %v", cmd.lines())
	}
}

// gateway-restart is a planned restart: agent-terminate's SIGTERM, with the
// restart policy pinned so docker does not revive the node on its own and
// put back once the container is started again — never `docker restart`,
// whose 10 s grace SIGKILLs a draining agent. It has no hold, so inject and
// restore run back to back and the restore re-wires the returned node.
func TestGatewayRestartRecyclesTheContainerAndRestoresTheNode(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())
	act := actionNamed(t, "gateway-restart")

	if err := act.inject(context.Background(), l, workloadHost, 0); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := act.restore(context.Background(), l, workloadHost); err != nil {
		t.Fatalf("restore: %v", err)
	}

	last := -1
	for _, step := range []string{
		"docker update --restart=no clab-ovn-e2e-gateway-3",
		"docker exec clab-ovn-e2e-gateway-3 kill -TERM 1",
		"docker inspect -f {{.State.Running}} clab-ovn-e2e-gateway-3",
		"docker start clab-ovn-e2e-gateway-3",
		"docker update --restart=always clab-ovn-e2e-gateway-3",
	} {
		at := cmd.indexOf(step)
		if at <= last {
			t.Fatalf("the restart never issued %q, or issued it out of order: %v", step, cmd.lines())
		}
		last = at
	}
	if cmd.called("docker restart") {
		t.Fatalf("`docker restart` SIGKILLs a draining agent after 10 s: %v", cmd.lines())
	}
	rewire := cmd.indexOf("containerlab tools veth create")
	responder := cmd.indexOf("external_ids:iface-id=ls0-vm1")
	if rewire < 0 || responder < 0 {
		t.Fatalf("restore did not re-wire the underlay and rebuild the responders: %v", cmd.lines())
	}
	if rewire > responder {
		t.Fatalf("the responders were rebuilt before the underlay came back: %v", cmd.lines())
	}
}

// A container lifecycle event on the workload host destroys its network
// namespace: every responder behind a FIP and the port-forward backend
// have to be re-created. No other gateway carries node-local workloads.
func TestReprovisionRebuildsTheWorkloadHostOnly(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := reprovisionNode(context.Background(), l, defaultTestProfile(t), workloadHost); err != nil {
		t.Fatalf("reprovision %s: %v", workloadHost, err)
	}

	for _, want := range []string{
		"external_ids:iface-id=ls0-vm1",
		"external_ids:iface-id=ls0-vm2",
		"external_ids:iface-id=vm101",
		"external_ids:iface-id=vm102",
		"/usr/local/bin/pf-backend -addr :8080 -log /tmp/pf-backend.log",
	} {
		if !cmd.called(want) {
			t.Fatalf("reprovision did not restore %q: %v", want, cmd.lines())
		}
	}

	peer := &fakeCommander{respond: healthyLabResponses}
	if err := reprovisionNode(context.Background(), newTestLab(peer, newFakeClock()),
		defaultTestProfile(t), "gateway-2"); err != nil {
		t.Fatalf("reprovision gateway-2: %v", err)
	}
	if len(peer.lines()) != 0 {
		t.Fatalf("reprovision touched a gateway with no node-local workloads: %v", peer.lines())
	}
}

// The port-forward backend is restarted, not started alongside a stale
// instance, and the run waits for it to bind before probing the VIP.
func TestStartPFBackendReplacesAnyRunningInstance(t *testing.T) {
	cmd := &fakeCommander{respond: healthyLabResponses}
	l := newTestLab(cmd, newFakeClock())

	if err := startPFBackend(context.Background(), l); err != nil {
		t.Fatalf("startPFBackend: %v", err)
	}

	kill := cmd.indexOf("pkill -f " + pfBackendLog)
	start := cmd.indexOf("exec -d clab-ovn-e2e-gateway-3")
	listen := cmd.indexOf("sport = :8080")
	if kill < 0 || start < 0 || listen < 0 {
		t.Fatalf("backend was not reset, started and waited for: %v", cmd.lines())
	}
	if kill > start || start > listen {
		t.Fatalf("backend steps ran out of order: %v", cmd.lines())
	}
}

// A backend that never binds must fail the layering rather than let the
// VIP probe report a false violation for the rest of the run.
func TestStartPFBackendFailsWhenTheListenerNeverBinds(t *testing.T) {
	cmd := &fakeCommander{respond: func(argv []string) (string, error) {
		if strings.Contains(strings.Join(argv, " "), "sport = :8080") {
			return "", errBoom
		}
		return "", nil
	}}
	l := newTestLab(cmd, newFakeClock())

	err := startPFBackend(context.Background(), l)
	if err == nil {
		t.Fatal("a backend that never bound was accepted")
	}
	if !strings.Contains(err.Error(), "did not bind") {
		t.Fatalf("error %q does not say the listener never came up", err)
	}
}

// The VLAN provider networks the start state layers on must land with
// their localnet tag and their router pinned, or the FIPs behind them
// never answer and every fault reads as a violation.
func TestVLANLayerTagsTheLocalnetPortAndPinsTheRouter(t *testing.T) {
	cmd := &fakeCommander{}
	l := newTestLab(cmd, newFakeClock())

	if err := applyVLANLayer(context.Background(), l, vlanNetworks[0]); err != nil {
		t.Fatalf("applyVLANLayer: %v", err)
	}

	for _, want := range []string{
		"lsp-set-options ln-vlan101 network_name=physnet1",
		"set Logical_Switch_Port ln-vlan101 tag=101",
		"lrp-set-gateway-chassis lr-vlan101-public gateway-1 30",
		"lr-nat-add lr-vlan101 dnat_and_snat 198.51.100.10 192.168.101.10",
	} {
		if !cmd.called(want) {
			t.Fatalf("vlan layer did not issue %q: %v", want, cmd.lines())
		}
	}
}

func TestVLANLayerReportsANBFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	if err := applyVLANLayer(context.Background(), l, vlanNetworks[1]); err == nil {
		t.Fatal("a failed ovn-nbctl call was swallowed")
	}
}

func TestCrossChassisLayerReportsANBFailure(t *testing.T) {
	cmd := &fakeCommander{respond: func([]string) (string, error) { return "", errBoom }}
	l := newTestLab(cmd, newFakeClock())

	if err := applyCrossChassisLayer(context.Background(), l); err == nil {
		t.Fatal("a failed ovn-nbctl call was swallowed")
	}
}

// 192.0.2.11 is seeded by bootstrap.sh as a NAT row with nothing behind
// it. Probing it would be red for the whole run and every recovery gate
// would time out, so it must stay out of the probe set.
func TestProbeTargetsExcludeTheBackendlessFIP(t *testing.T) {
	for _, target := range defaultProbes {
		if strings.Contains(target.addr, "192.0.2.11") {
			t.Fatalf("probe target %q has no responder behind it", target.name)
		}
	}
	if len(defaultProbes) != 7 {
		t.Fatalf("probe set = %v, want the four FIPs, the port-forward VIP, the same-node hairpin FIP and the cross-chassis FIP", defaultProbes)
	}
}

// The registry order and the weights are the replay contract: a new action
// is appended, never inserted, so a recorded seed still replays the
// sequence it recorded. This asserts the full ordered set and every
// action's own invariants.
func TestActionRegistryOrderIsStable(t *testing.T) {
	actions := fullRegistry(t)

	want := []string{
		// starter faults (issue #176) and the config change (issue #177)
		"controller-restart", "gateway-kill", "agent-terminate",
		"gateway-restart", "config-flip",
		// control-plane outages (issue #178)
		"nb-pause", "sb-pause", "northd-pause", "double-failover",
		// network impairment (issue #178)
		"mgmt-loss", "mgmt-delay",
		// data-plane drift (issue #178)
		"kernel-route-drop", "frr-route-drop", "nft-flush", "ovs-flow-drop",
		// routing flaps (issue #178)
		"frr-restart", "upstream-bgp-restart",
		// OVN churn (issue #178)
		"fip-churn", "lb-vip-churn", "priority-flip", "chassis-delete",
	}
	got := actionOrder(actions)
	if len(got) != len(want) {
		t.Fatalf("registry = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry = %v, want %v", got, want)
		}
	}
	for _, a := range actions {
		if a.weight <= 0 {
			t.Fatalf("action %s ships with weight %d — the weighted pick would never draw it", a.name, a.weight)
		}
		if a.holdMax < a.holdMin {
			t.Fatalf("action %s has holdMax %s below holdMin %s", a.name, a.holdMax, a.holdMin)
		}
		if a.recoveryBudget <= 0 {
			t.Fatalf("action %s has no recovery budget", a.name)
		}
	}

	// config-flip is the one action whose behaviour depends on the drawn
	// flip, and its restart onto the new config is its own undo.
	flip := actionFromRegistry(t, actions, "config-flip")
	if !flip.usesFlip || flip.applicable == nil {
		t.Fatal("config-flip does not read the flip index the engine draws for it")
	}
	if flip.holdMin != 0 || flip.holdMax != 0 {
		t.Fatalf("config-flip holds its fault for %s–%s", flip.holdMin, flip.holdMax)
	}
}

// fullRegistry builds the registry the runner drives, with the dependencies
// the actions bind to. A nil lab is fine: the actions capture it but only
// touch it at run time, and no test here executes one.
func fullRegistry(t *testing.T) []*action {
	t.Helper()
	p := defaultTestProfile(t)
	return allActions(p, nil, newApplier(nil, p, nil), newChurner(nil))
}

func actionOrder(actions []*action) []string {
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		names = append(names, a.name)
	}
	return names
}

func actionFromRegistry(t *testing.T, actions []*action, name string) *action {
	t.Helper()
	for _, a := range actions {
		if a.name == name {
			return a
		}
	}
	t.Fatalf("no action named %q in the registry: %v", name, actionOrder(actions))
	return nil
}

// The guardrail is consulted before the fault is injected, and an applier
// that has not put a profile on the lab yet tracks no configuration — so
// it reports every flip as inapplicable rather than flipping a
// configuration it has never read. (run() applies the profile before the
// engine draws anything.)
func TestAnApplierWithoutAProfileSkipsEveryFlip(t *testing.T) {
	ap := newApplier(nil, defaultTestProfile(t), nil)

	for i := range flips() {
		if ap.applicable(context.Background(), "gateway-1", i) {
			t.Fatalf("flip %s was applicable before the profile was applied", flipName(i))
		}
	}
}

// The config-flip restore puts back only a node the flip restarted. After
// a reload or a rejected flip the gateway never went down, and re-wiring it
// would take its underlay link and BGP session down for nothing.
func TestConfigFlipRestoresOnlyANodeTheFlipRestarted(t *testing.T) {
	tests := []struct {
		name        string
		flip        string
		verdict     string
		wantRestore bool
	}{
		{name: "a reload", flip: "cadence-toggle", verdict: "success"},
		{name: "a refused reload", flip: "cadence-toggle", verdict: "error"},
		{name: "a restart", flip: "drain-toggle", verdict: "success", wantRestore: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &fakeCommander{respond: reloadingLab(string(baseConfig(t)), tc.verdict)}
			a, _, _ := flipOnto(t, cmd)
			act := actionFromRegistry(t,
				allActions(a.profile, a.lab, a, newChurner(a.lab)), "config-flip")

			if err := act.inject(context.Background(), a.lab, "gateway-1", flipIndex(t, tc.flip)); err != nil {
				t.Fatalf("inject: %v", err)
			}
			before := len(cmd.lines())
			if err := act.restore(context.Background(), a.lab, "gateway-1"); err != nil {
				t.Fatalf("restore: %v", err)
			}

			restored := cmd.lines()[before:]
			rewired := strings.Contains(strings.Join(restored, "\n"), "containerlab tools veth create")
			if rewired != tc.wantRestore {
				t.Fatalf("after %s the restore re-wired = %v, want %v: %v", tc.name, rewired, tc.wantRestore, restored)
			}
			if !tc.wantRestore && len(restored) != 0 {
				t.Fatalf("after %s the restore touched a live gateway: %v", tc.name, restored)
			}
		})
	}
}
