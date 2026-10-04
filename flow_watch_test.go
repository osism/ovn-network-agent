package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	hairpinDeleteLine   = " event=DELETED reason=delete table=0 cookie=0x998 ip,in_port=1,nw_dst=192.0.2.10"
	hairpinDeleteLine6  = " event=DELETED reason=delete table=0 cookie=0x998 ipv6,in_port=1,ipv6_dst=2001:db8::10"
	macTweakDeleteLine  = " event=DELETED reason=delete table=0 cookie=0x999 ip,in_port=1"
	hairpinIdleLine     = " event=DELETED reason=idle table=0 idle_timeout=5 cookie=0x998 ip,in_port=1,nw_dst=192.0.2.10"
	flowMonitorHeader   = "NXST_FLOW_MONITOR reply (xid=0x2):"
	flowDriftMetric     = "ovn_network_agent_ovs_flow_drift_total"
	unparseableFlowLine = "ignoring unparseable OVS flow monitor line"
)

var (
	hairpinDeleteKey  = flowKey{priority: hairpinFlowPriority, inPort: "1", dst: "192.0.2.10"}
	macTweakDeleteKey = flowKey{priority: macTweakFlowPriority, inPort: "1"}
)

func TestParseFlowMonitorLine(t *testing.T) {
	accepted := []struct {
		name string
		line string
		want flowEvent
	}{
		{"IPv4 hairpin deletion", hairpinDeleteLine, flowEvent{Plane: flowPlaneHairpin, Key: hairpinDeleteKey}},
		{"IPv6 hairpin deletion", hairpinDeleteLine6, flowEvent{Plane: flowPlaneHairpin, Key: flowKey{910, true, "1", "2001:db8::10"}}},
		{"MAC-tweak deletion", macTweakDeleteLine, flowEvent{Plane: flowPlaneMACTweak, Key: flowKey{900, false, "1", ""}}},
		{"idle timeout is drift like a delete", hairpinIdleLine, flowEvent{Plane: flowPlaneHairpin, Key: hairpinDeleteKey}},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			got, ok := parseFlowMonitorLine(tc.line)
			if !ok || got != tc.want {
				t.Errorf("parseFlowMonitorLine(%q) = (%+v, %v), want (%+v, true)", tc.line, got, ok, tc.want)
			}
			if strings.Contains(logs.String(), unparseableFlowLine) {
				t.Errorf("an accepted line was logged as unparseable:\n%s", logs)
			}
		})
	}

	rejected := []struct {
		name     string
		line     string
		warnings int
	}{
		{name: "empty line", line: ""},
		{name: "reply header", line: "NXST_FLOW_MONITOR reply (xid=0x0):"},
		{name: "addition with an agent cookie", line: " event=ADDED table=0 cookie=0x998 ip,in_port=1,nw_dst=192.0.2.10"},
		{name: "deletion with cookie 0", line: " event=DELETED reason=delete table=0 cookie=0"},
		{name: "deletion with a foreign cookie", line: " event=DELETED reason=delete table=0 cookie=0x1 ip,in_port=1,nw_dst=192.0.2.10"},
		{name: "deletion in another table", line: " event=DELETED reason=delete table=1 cookie=0x998 ip,in_port=1,nw_dst=192.0.2.10"},
		{name: "process message", line: "ovs-ofctl: vconn_recv (End of file)"},
		{name: "named in_port", line: " event=DELETED reason=delete table=0 cookie=0x998 ip,in_port=patch-provnet,nw_dst=192.0.2.10", warnings: 1},
		{name: "bogus destination", line: " event=DELETED reason=delete table=0 cookie=0x998 ip,in_port=1,nw_dst=bogus", warnings: 1},
		{name: "no match after the cookie", line: " event=DELETED reason=delete table=0 cookie=0x998", warnings: 1},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			if got, ok := parseFlowMonitorLine(tc.line); ok {
				t.Errorf("parseFlowMonitorLine(%q) = (%+v, true), want false", tc.line, got)
			}
			if got := strings.Count(logs.String(), unparseableFlowLine); got != tc.warnings {
				t.Errorf("unparseable-line warnings = %d, want %d:\n%s", got, tc.warnings, logs)
			}
		})
	}
}

// A deletion counts as drift only when its key is one the reconcile
// published, so a line has to key to exactly what the desired flows render.
func TestParseFlowMonitorLineMatchesDesiredKeys(t *testing.T) {
	rm := &RouteManager{segments: fallbackSegments("patch-provnet-0", "1", "aa:bb:cc:dd:ee:ff")}
	desired := append(rm.desiredHairpinFlows(map[string]HairpinTarget{
		"192.0.2.10":   {RouterMAC: "fa:16:3e:00:00:01"},
		"2001:db8::10": {RouterMAC: "fa:16:3e:00:00:01"},
	}), rm.desiredMACTweakFlows()...)
	want := make(map[flowKey]bool, len(desired))
	for _, d := range desired {
		want[d.key] = true
	}

	for _, line := range []string{
		hairpinDeleteLine,
		hairpinDeleteLine6,
		macTweakDeleteLine,
		" event=DELETED reason=delete table=0 cookie=0x999 ipv6,in_port=1",
	} {
		ev, ok := parseFlowMonitorLine(line)
		if !ok {
			t.Fatalf("parseFlowMonitorLine(%q) rejected the line", line)
		}
		if !want[ev.Key] {
			t.Errorf("parseFlowMonitorLine(%q) key %+v is not one of the desired keys %v", line, ev.Key, want)
		}
	}
}

// The parser reads another process's output. Whatever it is fed, it must not
// panic, and an event it accepts has to name a plane with that plane's
// priority.
func FuzzParseFlowMonitorLine(f *testing.F) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	f.Cleanup(func() { slog.SetDefault(prev) })

	for _, line := range []string{
		hairpinDeleteLine, hairpinDeleteLine6, macTweakDeleteLine, hairpinIdleLine,
		flowMonitorHeader, "", "ovs-ofctl: vconn_recv (End of file)",
		" event=DELETED reason=delete table=0 cookie=0x998 ip,in_port=patch-provnet,nw_dst=192.0.2.10",
	} {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		ev, ok := parseFlowMonitorLine(line)
		if !ok {
			return
		}
		want := map[string]int{flowPlaneHairpin: hairpinFlowPriority, flowPlaneMACTweak: macTweakFlowPriority}
		if priority, known := want[ev.Plane]; !known || ev.Key.priority != priority {
			t.Errorf("parseFlowMonitorLine(%q) = %+v: unknown plane or not its priority", line, ev)
		}
	})
}

func TestFlowPlaneForCookie(t *testing.T) {
	cases := []struct {
		cookie       string
		wantPlane    string
		wantPriority int
		wantOK       bool
	}{
		{ovsCookieHairpin, flowPlaneHairpin, hairpinFlowPriority, true},
		{ovsCookieMACTweak, flowPlaneMACTweak, macTweakFlowPriority, true},
		{"0x1", "", 0, false},
		{"", "", 0, false},
	}
	for _, tc := range cases {
		plane, priority, ok := flowPlaneForCookie(tc.cookie)
		if plane != tc.wantPlane || priority != tc.wantPriority || ok != tc.wantOK {
			t.Errorf("flowPlaneForCookie(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tc.cookie, plane, priority, ok, tc.wantPlane, tc.wantPriority, tc.wantOK)
		}
	}
}

// =============================================================================
// Ownership and the watcher, with events injected through subscribe
// =============================================================================

// ownedFlows returns the desired flows of one plane that carry keys.
func ownedFlows(keys ...flowKey) []desiredFlow {
	flows := make([]desiredFlow, 0, len(keys))
	for _, k := range keys {
		flows = append(flows, desiredFlow{key: k})
	}
	return flows
}

func TestFlowWatcherOwnership(t *testing.T) {
	t.Run("a watcher that never published owns nothing", func(t *testing.T) {
		w := newFlowWatcher(Config{}, func() {})
		if w.owns(flowPlaneHairpin, hairpinDeleteKey) || w.owns(flowPlaneMACTweak, macTweakDeleteKey) {
			t.Error("owns() = true before setOwned was ever called")
		}
		var zero flowWatcher
		if zero.owns(flowPlaneHairpin, hairpinDeleteKey) {
			t.Error("owns() = true on a zero flowWatcher")
		}
	})

	t.Run("one plane is replaced without touching the other", func(t *testing.T) {
		w := newFlowWatcher(Config{}, func() {})
		w.setOwned(flowPlaneHairpin, ownedFlows(hairpinDeleteKey))
		w.setOwned(flowPlaneMACTweak, ownedFlows(macTweakDeleteKey))

		w.setOwned(flowPlaneHairpin, nil)

		if w.owns(flowPlaneHairpin, hairpinDeleteKey) {
			t.Error("the hairpin key is still owned after its plane was cleared")
		}
		if !w.owns(flowPlaneMACTweak, macTweakDeleteKey) {
			t.Error("clearing the hairpin plane dropped the MAC-tweak key")
		}
	})

	t.Run("a key is owned only on its own plane", func(t *testing.T) {
		w := newFlowWatcher(Config{}, func() {})
		w.setOwned(flowPlaneHairpin, ownedFlows(hairpinDeleteKey))
		if w.owns(flowPlaneMACTweak, hairpinDeleteKey) {
			t.Error("a hairpin key is owned on the MAC-tweak plane")
		}
	})
}

// newTestFlowWatcher builds a watcher that owns the test deletions on both
// planes, subscribes through the script and runs on short timings.
func newTestFlowWatcher(script *subscribeScript[flowEvent], triggers *triggerLog) *flowWatcher {
	w := newFlowWatcher(Config{BridgeDev: "br-ex"}, triggers.trigger)
	w.subscribe = script.subscribe
	w.debounce = 20 * time.Millisecond
	w.minInterval = time.Millisecond
	w.backoffMin = 10 * time.Millisecond
	w.backoffMax = 40 * time.Millisecond
	w.setOwned(flowPlaneHairpin, ownedFlows(hairpinDeleteKey))
	w.setOwned(flowPlaneMACTweak, ownedFlows(macTweakDeleteKey))
	return w
}

func TestFlowWatcherDebouncesDriftIntoOneTrigger(t *testing.T) {
	m := withTestMetrics(t)
	logs := captureSlog(t)
	events := make(chan flowEvent)
	script := &subscribeScript[flowEvent]{results: []subscribeResult[flowEvent]{{events: events}}}
	triggers := &triggerLog{}
	w := newTestFlowWatcher(script, triggers)
	w.debounce = 100 * time.Millisecond
	stop := startWatcher(t, w)

	sendEvent(t, events, flowEvent{Plane: flowPlaneHairpin, Key: hairpinDeleteKey})
	sendEvent(t, events, flowEvent{Plane: flowPlaneMACTweak, Key: macTweakDeleteKey})
	sendEvent(t, events, flowEvent{Plane: flowPlaneHairpin, Key: hairpinDeleteKey})

	waitForCondition(t, "the debounced trigger", func() bool { return triggers.count() > 0 })
	time.Sleep(2 * w.debounce)
	stop()
	if got := triggers.count(); got != 1 {
		t.Errorf("triggers = %d, want 1 for three deletions inside the debounce window", got)
	}
	if got := counterValue(t, m, flowDriftMetric, "plane", flowPlaneHairpin); got != 2 {
		t.Errorf("%s{plane=\"hairpin\"} = %v, want 2", flowDriftMetric, got)
	}
	if got := counterValue(t, m, flowDriftMetric, "plane", flowPlaneMACTweak); got != 1 {
		t.Errorf("%s{plane=\"mactweak\"} = %v, want 1", flowDriftMetric, got)
	}
	if want := `msg="OVS flow drift detected, reconciling" mactweak=1 hairpin=2`; !strings.Contains(logs.String(), want) {
		t.Errorf("the trigger line does not list the counts per plane, want %q in:\n%s", want, logs)
	}
}

func TestFlowWatcherIgnoresUnownedKey(t *testing.T) {
	m := withTestMetrics(t)
	events := make(chan flowEvent)
	script := &subscribeScript[flowEvent]{results: []subscribeResult[flowEvent]{{events: events}}}
	triggers := &triggerLog{}
	w := newTestFlowWatcher(script, triggers)
	startWatcher(t, w)

	unowned := hairpinDeleteKey
	unowned.dst = "192.0.2.99"
	sendEvent(t, events, flowEvent{Plane: flowPlaneHairpin, Key: unowned})
	// The MAC-tweak key is owned on its own plane only.
	sendEvent(t, events, flowEvent{Plane: flowPlaneHairpin, Key: macTweakDeleteKey})

	time.Sleep(2 * w.debounce)
	if got := triggers.count(); got != 0 {
		t.Errorf("triggers = %d, want 0 for keys that are not owned", got)
	}
	for _, plane := range []string{flowPlaneHairpin, flowPlaneMACTweak} {
		if got := counterValue(t, m, flowDriftMetric, "plane", plane); got != 0 {
			t.Errorf("%s{plane=%q} = %v, want 0", flowDriftMetric, plane, got)
		}
	}
}

func TestFlowWatcherLogsOneOutageAndTriggersOnRecovery(t *testing.T) {
	withTestMetrics(t)
	logs := captureSlog(t)
	failed := errors.New("start the OVS flow monitor on br-ex: exec: no such file")
	script := &subscribeScript[flowEvent]{results: []subscribeResult[flowEvent]{
		{err: failed},
		{err: failed},
		{events: make(chan flowEvent)},
	}}
	triggers := &triggerLog{}
	w := newTestFlowWatcher(script, triggers)
	stop := startWatcher(t, w)

	waitForCondition(t, "the trigger on recovery", func() bool { return triggers.count() > 0 })
	time.Sleep(2 * w.backoffMax)
	stop()

	if got := triggers.count(); got != 1 {
		t.Errorf("triggers = %d, want 1 on recovery", got)
	}
	if got := script.callCount(); got != 3 {
		t.Errorf("subscribe calls = %d, want 3", got)
	}
	if got := strings.Count(logs.String(), "OVS flow watch unavailable"); got != 1 {
		t.Errorf("outage warnings = %d, want 1 for one outage:\n%s", got, logs)
	}
	if !strings.Contains(logs.String(), failed.Error()) {
		t.Errorf("the outage warning does not carry the subscribe error:\n%s", logs)
	}
	if got := strings.Count(logs.String(), "OVS flow watch is back"); got != 1 {
		t.Errorf("recovery lines = %d, want 1:\n%s", got, logs)
	}
}

func TestFlowWatcherReportsAnEndedMonitor(t *testing.T) {
	withTestMetrics(t)
	logs := captureSlog(t)
	first := make(chan flowEvent)
	script := &subscribeScript[flowEvent]{results: []subscribeResult[flowEvent]{{events: first}}}
	triggers := &triggerLog{}
	w := newTestFlowWatcher(script, triggers)
	stop := startWatcher(t, w)

	waitForCondition(t, "the first subscribe", func() bool { return script.callCount() == 1 })
	close(first)
	waitForCondition(t, "the trigger on recovery", func() bool { return triggers.count() == 1 })
	stop()

	if !strings.Contains(logs.String(), errFlowMonitorEnded.Error()) {
		t.Errorf("the outage warning does not say the monitor ended:\n%s", logs)
	}
}

func TestFlowWatcherNilReceiverIsNoOp(t *testing.T) {
	var w *flowWatcher
	ok := returnsWithin(2*time.Second, func() {
		w.setOwned(flowPlaneHairpin, ownedFlows(hairpinDeleteKey))
		w.start(context.Background())
		w.wait()
	})
	if !ok {
		t.Fatal("a method of a nil flowWatcher blocked")
	}
}

func TestFlowWatcherWaitReturnsWhenNeverStarted(t *testing.T) {
	w := newFlowWatcher(Config{}, func() {})
	if !returnsWithin(2*time.Second, w.wait) {
		t.Fatal("wait() blocked on a watcher that was never started")
	}
}

// A zero backoff would turn a monitor that cannot start into a busy loop, and
// a zero debounce or rate limit would reconcile once per deleted flow.
func TestNewFlowWatcherUsesProductionSettings(t *testing.T) {
	w := newFlowWatcher(Config{BridgeDev: "br-ex", OVSWrapper: " docker  exec -i openvswitch_vswitchd "}, func() {})

	if w.debounce != driftDebounce || w.minInterval != driftMinInterval {
		t.Errorf("debounce = %v, minInterval = %v, want %v and %v",
			w.debounce, w.minInterval, driftDebounce, driftMinInterval)
	}
	if w.backoffMin != driftMinBackoff || w.backoffMax != driftMaxBackoff {
		t.Errorf("backoff = %v to %v, want %v to %v",
			w.backoffMin, w.backoffMax, driftMinBackoff, driftMaxBackoff)
	}
	m := w.monitor
	if m.bridge != "br-ex" || strings.Join(m.wrapper, "|") != "docker|exec|-i|openvswitch_vswitchd" {
		t.Errorf("monitor bridge = %q, wrapper = %q, want br-ex behind docker exec -i openvswitch_vswitchd", m.bridge, m.wrapper)
	}
	if m.ctl != ovsFlowMonitorCtl || m.startTimeout != flowMonitorStartTimeout || m.stopTimeout != flowMonitorStopTimeout {
		t.Errorf("monitor ctl = %q, timeouts = %v and %v, want %q, %v and %v",
			m.ctl, m.startTimeout, m.stopTimeout, ovsFlowMonitorCtl, flowMonitorStartTimeout, flowMonitorStopTimeout)
	}
}

// =============================================================================
// The monitor process, played by a wrapper script
// =============================================================================

// fakeOVS is a wrapper script that plays ovs-appctl and ovs-ofctl. It records
// every call, one line per argv, and the pid of the last monitor it started.
type fakeOVS struct {
	dir     string
	wrapper []string
}

// installFakeOVS writes the wrapper script. appctl and ofctl are the shell
// code each command runs. The wrapper carries a marker argument the way
// "docker exec <container>" carries its container, so a recorded call shows
// that the wrapper's own arguments come first.
func installFakeOVS(t *testing.T, appctl, ofctl string) *fakeOVS {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + dir + `/calls"
shift
case "$1" in
ovs-appctl)
` + appctl + `
;;
ovs-ofctl)
echo $$ > "` + dir + `/monitor.pid"
` + ofctl + `
;;
esac
`
	bin := filepath.Join(dir, "ovs-wrapper")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake OVS wrapper: %v", err)
	}
	return &fakeOVS{dir: dir, wrapper: []string{bin, "in-ovs"}}
}

// Shell code for the fake commands.
const (
	// appctlNoMonitor is ovs-appctl with nothing listening on the socket.
	appctlNoMonitor = `echo "ovs-appctl: cannot connect to socket" >&2; exit 1`
	// appctlEndsMonitor ends the monitor the script started last.
	appctlEndsMonitor = `if [ -f "$(dirname "$0")/monitor.pid" ]; then kill "$(cat "$(dirname "$0")/monitor.pid")"; exit 0; fi; exit 1`
	// ofctlHeaderOnly answers the request and then waits for events.
	ofctlHeaderOnly = `echo "` + flowMonitorHeader + `"; exec sleep 30`
)

// monitor returns a flowMonitor on br-ex behind the script.
func (f *fakeOVS) monitor() *flowMonitor {
	return &flowMonitor{
		wrapper:      f.wrapper,
		bridge:       "br-ex",
		ctl:          filepath.Join(f.dir, "monitor.ctl"),
		startTimeout: 2 * time.Second,
		stopTimeout:  2 * time.Second,
	}
}

// calls returns the recorded calls without the wrapper's marker argument.
func (f *fakeOVS) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil {
		t.Fatalf("read the recorded calls: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// monitorPid returns the pid of the last monitor the script started.
func (f *fakeOVS) monitorPid(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "monitor.pid"))
	if err != nil {
		t.Fatalf("read the monitor pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse the monitor pid %q: %v", data, err)
	}
	return pid
}

// endSubscription closes done and waits until the channel is closed and every
// process and goroutine of the monitor has finished, so a test reads the
// captured log only after nothing writes to it any more.
func endSubscription(t *testing.T, m *flowMonitor, done chan struct{}, events <-chan flowEvent, within time.Duration) {
	t.Helper()
	close(done)
	if events != nil {
		drainUntilClosed(t, events)
	}
	if !returnsWithin(within, m.reaped.Wait) {
		t.Fatalf("the monitor was not reaped within %v of closing done", within)
	}
}

// drainUntilClosed reads events until the channel is closed and fails the
// test after 2 s.
func drainUntilClosed(t *testing.T, events <-chan flowEvent) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("the event channel was not closed within 2s")
		}
	}
}

// processGone reports whether no process with pid exists any more.
func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestFlowMonitorStartsBehindTheWrapperAfterAnExit(t *testing.T) {
	logs := captureSlog(t)
	f := installFakeOVS(t, appctlNoMonitor, ofctlHeaderOnly)
	m := f.monitor()
	done := make(chan struct{})

	events, err := m.subscribe(done)
	if err != nil {
		t.Fatalf("subscribe() error: %v (a failed exit must not stop the start)", err)
	}
	calls := f.calls(t)
	endSubscription(t, m, done, events, 2*time.Second)

	exit := "in-ovs ovs-appctl -t " + m.ctl + " exit"
	start := "in-ovs ovs-ofctl --no-names --unixctl=" + m.ctl + " monitor br-ex watch:!initial,!add,!modify,!actions"
	if len(calls) != 2 || calls[0] != exit || calls[1] != start {
		t.Errorf("calls = %q, want the exit %q and then the start %q", calls, exit, start)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a monitor that was stopped on purpose logged a warning:\n%s", logs)
	}
}

func TestFlowMonitorDeliversDeletions(t *testing.T) {
	captureSlog(t)
	f := installFakeOVS(t, appctlNoMonitor, `echo "`+flowMonitorHeader+`"
echo "NXST_FLOW_MONITOR reply (xid=0x0):" >&2
echo "`+hairpinDeleteLine+`" >&2
exec sleep 30`)
	m := f.monitor()
	done := make(chan struct{})

	events, err := m.subscribe(done)
	if err != nil {
		t.Fatalf("subscribe() error: %v", err)
	}
	select {
	case ev, ok := <-events:
		if !ok || ev != (flowEvent{Plane: flowPlaneHairpin, Key: hairpinDeleteKey}) {
			t.Errorf("event = (%+v, %v), want the hairpin deletion", ev, ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event within 2s")
	}
	select {
	case ev, ok := <-events:
		t.Errorf("a second event = (%+v, %v), want none: the monitor reported one deletion", ev, ok)
	case <-time.After(100 * time.Millisecond):
	}
	endSubscription(t, m, done, events, 2*time.Second)
}

func TestFlowMonitorWithoutEventsStaysOpen(t *testing.T) {
	captureSlog(t)
	f := installFakeOVS(t, appctlNoMonitor, ofctlHeaderOnly)
	m := f.monitor()
	done := make(chan struct{})

	events, err := m.subscribe(done)
	if err != nil {
		t.Fatalf("subscribe() error: %v", err)
	}
	select {
	case ev, ok := <-events:
		t.Errorf("event = (%+v, %v), want nothing on a quiet bridge", ev, ok)
	case <-time.After(200 * time.Millisecond):
	}
	endSubscription(t, m, done, events, 2*time.Second)
}

func TestFlowMonitorOnAMissingBridge(t *testing.T) {
	captureSlog(t)
	const output = "ovs-ofctl: br-ex is not a bridge or a socket"
	f := installFakeOVS(t, appctlNoMonitor, `echo "`+output+`" >&2; exit 1`)
	m := f.monitor()
	done := make(chan struct{})

	events, err := m.subscribe(done)
	endSubscription(t, m, done, nil, 2*time.Second)

	if events != nil {
		t.Error("subscribe() returned a channel for a monitor that never became ready")
	}
	if err == nil {
		t.Fatal("subscribe() succeeded on a missing bridge")
	}
	if prefix := "OVS flow monitor on br-ex ended before it was ready:"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %q does not start with %q", err, prefix)
	}
	if !strings.Contains(err.Error(), output) {
		t.Errorf("error %q does not carry the monitor's output %q", err, output)
	}
}

func TestFlowMonitorWrapperThatDoesNotExist(t *testing.T) {
	captureSlog(t)
	m := &flowMonitor{
		wrapper:      []string{filepath.Join(t.TempDir(), "no-such-wrapper")},
		bridge:       "br-ex",
		ctl:          filepath.Join(t.TempDir(), "monitor.ctl"),
		startTimeout: time.Second,
		stopTimeout:  time.Second,
	}

	_, err := m.subscribe(make(chan struct{}))
	if err == nil {
		t.Fatal("subscribe() succeeded without a wrapper to run")
	}
	if prefix := "start the OVS flow monitor on br-ex:"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("error %q does not start with %q", err, prefix)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %q does not wrap the exec error", err)
	}
	if !returnsWithin(time.Second, m.reaped.Wait) {
		t.Error("a monitor that never started is waited for")
	}
}

func TestFlowMonitorNotReadyInTime(t *testing.T) {
	captureSlog(t)
	f := installFakeOVS(t, appctlNoMonitor, `exec sleep 30`)
	m := f.monitor()
	m.startTimeout = 200 * time.Millisecond
	done := make(chan struct{})

	_, err := m.subscribe(done)
	endSubscription(t, m, done, nil, 2*time.Second)

	if want := "OVS flow monitor on br-ex was not ready within 200ms"; err == nil || err.Error() != want {
		t.Errorf("subscribe() error = %v, want %q", err, want)
	}
	if pid := f.monitorPid(t); !processGone(pid) {
		t.Errorf("the monitor process %d is still running after the timeout", pid)
	}
}

func TestFlowMonitorThatEndsLogsOnce(t *testing.T) {
	logs := captureSlog(t)
	f := installFakeOVS(t, appctlNoMonitor, `echo "`+flowMonitorHeader+`"
echo "ovs-ofctl: vconn_recv (End of file)" >&2
exit 1`)
	m := f.monitor()
	done := make(chan struct{})

	events, err := m.subscribe(done)
	if err != nil {
		t.Fatalf("subscribe() error: %v", err)
	}
	drainUntilClosed(t, events)
	endSubscription(t, m, done, events, 2*time.Second)

	out := logs.String()
	if got := strings.Count(out, "OVS flow monitor ended"); got != 1 {
		t.Errorf("end warnings = %d, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "vconn_recv (End of file)") {
		t.Errorf("the end is not a warning carrying the monitor's last words:\n%s", out)
	}
}

// A line too long to read leaves nothing reading the monitor's output, so the
// monitor is ended and its subscription ends with it.
func TestFlowMonitorEndsOnAnUnreadableLine(t *testing.T) {
	captureSlog(t)
	f := installFakeOVS(t, appctlNoMonitor, `echo "`+flowMonitorHeader+`"
head -c 70000 /dev/zero | tr '\000' x >&2; echo >&2
exec sleep 30`)
	m := f.monitor()
	done := make(chan struct{})

	events, err := m.subscribe(done)
	if err != nil {
		t.Fatalf("subscribe() error: %v", err)
	}
	pid := f.monitorPid(t)
	drainUntilClosed(t, events)
	if !processGone(pid) {
		t.Errorf("the monitor process %d is still running after its output became unreadable", pid)
	}
	endSubscription(t, m, done, nil, 2*time.Second)
}

func TestFlowMonitorStopsOnDone(t *testing.T) {
	logs := captureSlog(t)
	f := installFakeOVS(t, appctlEndsMonitor, ofctlHeaderOnly)
	m := f.monitor()
	done := make(chan struct{})

	events, err := m.subscribe(done)
	if err != nil {
		t.Fatalf("subscribe() error: %v", err)
	}
	pid := f.monitorPid(t)
	endSubscription(t, m, done, events, 2*time.Second)

	calls := f.calls(t)
	if exit := "in-ovs ovs-appctl -t " + m.ctl + " exit"; len(calls) != 3 || calls[2] != exit {
		t.Errorf("calls = %q, want a closing %q", calls, exit)
	}
	if !processGone(pid) {
		t.Errorf("the monitor process %d is still running", pid)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a monitor that was stopped on purpose logged a warning:\n%s", logs)
	}
}

// Behind a wrapper the exit may not reach the monitor at all. The child is
// killed then, and the readers are released even while a descendant of the
// wrapper holds the pipes.
func TestFlowMonitorKillsAMonitorThatIgnoresExit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		appctl string
		ofctl  string
	}{
		{
			name:   "exit answered but ignored",
			appctl: `exit 0`,
			ofctl:  ofctlHeaderOnly,
		},
		{
			name:   "exit never answered, a descendant holds the pipes",
			appctl: `exec sleep 30`,
			ofctl:  `echo "` + flowMonitorHeader + `"; sleep 5 & exec sleep 30`,
		},
		{
			name:   "exit never answered, a descendant holds its output",
			appctl: `sleep 5 & exec sleep 30`,
			ofctl:  ofctlHeaderOnly,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureSlog(t)
			f := installFakeOVS(t, tc.appctl, tc.ofctl)
			m := f.monitor()
			m.stopTimeout = 300 * time.Millisecond
			done := make(chan struct{})

			events, err := m.subscribe(done)
			if err != nil {
				t.Fatalf("subscribe() error: %v", err)
			}
			pid := f.monitorPid(t)
			// The exit gives up after stopTimeout and the 1s it waits
			// for its output.
			endSubscription(t, m, done, events, m.stopTimeout+time.Second+2*time.Second)

			if !processGone(pid) {
				t.Errorf("the monitor process %d is still running", pid)
			}
		})
	}
}

// The stop of a subscription can answer late behind the wrapper. The next
// subscription waits for it, so that late exit cannot end the monitor started
// next.
func TestFlowMonitorResubscribeWaitsForThePreviousStop(t *testing.T) {
	captureSlog(t)
	// The second ovs-appctl call is the stop of the first subscription. It
	// answers 0.5s late and ends a monitor started in the meantime. The
	// first monitor ends on its own after its header, the next one stays up.
	f := installFakeOVS(t,
		`d=$(dirname "$0"); n=$(( $(cat "$d/appctl.n" 2>/dev/null || echo 0) + 1 )); echo $n > "$d/appctl.n"
if [ $n -eq 2 ]; then p=$(cat "$d/monitor.pid"); sleep 0.5; q=$(cat "$d/monitor.pid"); [ "$q" != "$p" ] && kill "$q"; fi; exit 1`,
		`d=$(dirname "$0"); echo "`+flowMonitorHeader+`"
if [ ! -f "$d/ofctl.ran" ]; then touch "$d/ofctl.ran"; exit 1; fi
exec sleep 30`)
	m := f.monitor()

	done1 := make(chan struct{})
	events1, err := m.subscribe(done1)
	if err != nil {
		t.Fatalf("first subscribe() error: %v", err)
	}
	drainUntilClosed(t, events1)
	close(done1)
	waitForCondition(t, "the stop of the first subscription", func() bool {
		n, _ := os.ReadFile(filepath.Join(f.dir, "appctl.n"))
		return strings.TrimSpace(string(n)) == "2"
	})

	done2 := make(chan struct{})
	events2, err := m.subscribe(done2)
	if err != nil {
		t.Fatalf("second subscribe() error: %v", err)
	}
	select {
	case _, ok := <-events2:
		if !ok {
			t.Error("the late stop of the first subscription ended the second monitor")
		}
	case <-time.After(time.Second):
	}
	endSubscription(t, m, done2, events2, 2*time.Second)
}

// The watcher joins every monitor it started: after wait the process is gone.
func TestFlowWatcherRunsTheMonitor(t *testing.T) {
	m := withTestMetrics(t)
	captureSlog(t)
	f := installFakeOVS(t, appctlEndsMonitor, `echo "`+flowMonitorHeader+`"
echo "`+macTweakDeleteLine+`" >&2
exec sleep 30`)
	triggers := &triggerLog{}
	w := newFlowWatcher(Config{BridgeDev: "br-ex"}, triggers.trigger)
	fake := f.monitor()
	w.monitor.wrapper, w.monitor.ctl = fake.wrapper, fake.ctl
	w.monitor.startTimeout, w.monitor.stopTimeout = fake.startTimeout, fake.stopTimeout
	w.setOwned(flowPlaneMACTweak, ownedFlows(macTweakDeleteKey))
	stop := startWatcher(t, w)

	waitForCondition(t, "the trigger for the deleted MAC-tweak flow", func() bool { return triggers.count() == 1 })
	pid := f.monitorPid(t)
	if !returnsWithin(2*time.Second, stop) {
		t.Fatal("wait() did not return after the context was cancelled")
	}

	if !processGone(pid) {
		t.Errorf("the monitor process %d is still running after wait()", pid)
	}
	if got := counterValue(t, m, flowDriftMetric, "plane", flowPlaneMACTweak); got != 1 {
		t.Errorf("%s{plane=\"mactweak\"} = %v, want 1", flowDriftMetric, got)
	}
}
