package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// The OVS flow watch keeps one "ovs-ofctl monitor" process on the provider
// bridge and makes the agent reconcile as soon as that process reports that a
// hairpin or MAC-tweak flow the agent owns was deleted, instead of leaving the
// flow missing until the next periodic reconcile. The goroutine that turns
// drift into a reconcile is the driftWatcher of drift_watch.go.
//
// A deleted bridge or a stopped ovs-vswitchd ends the monitor process. The
// watcher subscribes again with a backoff, and the subscription that comes
// back triggers the reconcile that puts the flows back. A re-created patch
// port deletes no flow, so the monitor reports nothing for it; the periodic
// reconcile re-validates the segment bindings and catches it.

// flowEvent is one flow deletion the monitor reported.
type flowEvent struct {
	Plane string  // flowPlaneHairpin or flowPlaneMACTweak, from the cookie
	Key   flowKey // identity of the deleted flow
}

// watchSubject names the OVS flow watch in its log lines.
func (flowEvent) watchSubject() string { return "OVS flow" }

// errFlowMonitorEnded is the error the outage warning carries when a
// subscription ended because its monitor process ended.
var errFlowMonitorEnded = errors.New("OVS flow monitor ended")

const (
	// ovsFlowMonitorCtl is the control socket of the monitor process. The
	// path is fixed, so a subscription can end a monitor that an earlier
	// subscription or an earlier agent process left behind.
	ovsFlowMonitorCtl = "/var/run/openvswitch/ovn-network-agent-flow-monitor.ctl"

	// ovsFlowMonitorSpec asks for deletions only. Without !initial the
	// monitor would list every flow at start, and without !add the agent's
	// own adds and in-place replaces, which OVS reports as ADDED, would
	// reach the watcher.
	ovsFlowMonitorSpec = "watch:!initial,!add,!modify,!actions"

	// flowMonitorStartTimeout bounds the wait for the monitor's reply
	// header. flowMonitorStopTimeout bounds the ovs-appctl call that ends
	// a monitor.
	flowMonitorStartTimeout = 5 * time.Second
	flowMonitorStopTimeout  = 5 * time.Second
)

// parseFlowMonitorLine turns one line the monitor printed on stderr into the
// deletion it reports, such as
//
//	event=DELETED reason=delete table=0 cookie=0x998 ip,in_port=1,nw_dst=192.0.2.10
//
// It returns false for every other line: reply headers, other events, other
// tables, cookies the agent does not own, and the process's own messages. The
// reason is not looked at, since an idle, hard or eviction removal is drift
// like a delete. The line carries no priority, so the event's key takes the
// priority its plane installs flows at.
//
// A deletion with an agent cookie whose match cannot be keyed is logged and
// dropped rather than guessed at.
func parseFlowMonitorLine(line string) (flowEvent, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "event=DELETED" || !slices.Contains(fields, "table=0") {
		return flowEvent{}, false
	}
	at := slices.IndexFunc(fields, func(f string) bool { return strings.HasPrefix(f, "cookie=") })
	if at < 0 {
		return flowEvent{}, false
	}
	plane, priority, ok := flowPlaneForCookie(strings.TrimPrefix(fields[at], "cookie="))
	if !ok {
		return flowEvent{}, false
	}

	key, _, ok := parseFlowMatch(line)
	if !ok {
		slog.Warn("ignoring unparseable OVS flow monitor line", "line", strings.TrimSpace(line))
		return flowEvent{}, false
	}
	key.priority = priority
	return flowEvent{Plane: plane, Key: key}, true
}

// flowWatcher turns the flow deletions the monitor reports into reconcile
// triggers. The embedded driftWatcher owns the subscription on one goroutine,
// started by start. The reconcile publishes the flows it wants per plane
// through setOwned.
//
// setOwned, start and wait are no-ops on a nil receiver, so an agent without a
// watcher (ovs_flow_watch off, dry-run, port-forward-only mode) calls them
// unguarded.
type flowWatcher struct {
	// mu guards owned.
	mu sync.Mutex
	// owned maps a plane to the keys the last reconcile of that plane
	// wanted installed.
	owned map[string]map[flowKey]bool

	monitor *flowMonitor

	driftWatcher[flowEvent]
}

// newFlowWatcher builds a watcher that calls trigger when an owned flow is
// deleted. It copies the bridge and the OVS wrapper out of cfg and keeps no
// Config: the watcher's goroutine must never read the agent's configuration,
// which a reload replaces from Run's goroutine.
func newFlowWatcher(cfg Config, trigger func()) *flowWatcher {
	w := &flowWatcher{
		monitor: &flowMonitor{
			wrapper:      strings.Fields(cfg.OVSWrapper),
			bridge:       cfg.BridgeDev,
			ctl:          ovsFlowMonitorCtl,
			startTimeout: flowMonitorStartTimeout,
			stopTimeout:  flowMonitorStopTimeout,
		},
	}
	w.driftWatcher = driftWatcher[flowEvent]{
		kinds:     []string{flowPlaneMACTweak, flowPlaneHairpin},
		closedErr: errFlowMonitorEnded,
		subscribe: w.monitor.subscribe,
		classify: func(ev flowEvent) (string, bool) {
			return ev.Plane, w.owns(ev.Plane, ev.Key)
		},
		onDrift: func(ev flowEvent, kind string) {
			recordOVSFlowDrift(kind)
			slog.Debug("OVS flow drift event", "plane", kind, "in_port", ev.Key.inPort, "dst", ev.Key.dst)
		},
		trigger:     trigger,
		debounce:    driftDebounce,
		minInterval: driftMinInterval,
		backoffMin:  driftMinBackoff,
		backoffMax:  driftMaxBackoff,
	}
	return w
}

// setOwned publishes the flows the current reconcile wants installed on one
// plane and leaves the other plane alone. The reconcile calls it before it
// dumps the plane, so a flow it deletes on purpose is no longer owned when
// the monitor reports the deletion.
func (w *flowWatcher) setOwned(plane string, flows []desiredFlow) {
	if w == nil {
		return
	}
	keys := make(map[flowKey]bool, len(flows))
	for _, f := range flows {
		keys[f.key] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.owned == nil {
		w.owned = make(map[string]map[flowKey]bool, 2)
	}
	w.owned[plane] = keys
}

// owns reports whether the last reconcile of plane wanted the flow with key
// installed.
func (w *flowWatcher) owns(plane string, key flowKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.owned[plane][key]
}

// start runs the watcher on its own goroutine until ctx is done.
func (w *flowWatcher) start(ctx context.Context) {
	if w == nil {
		return
	}
	w.driftWatcher.start(ctx)
}

// wait blocks until the goroutine has returned and every monitor process it
// started was reaped. It returns at once when start was never called.
func (w *flowWatcher) wait() {
	if w == nil {
		return
	}
	w.driftWatcher.wait()
	w.monitor.reaped.Wait()
}

// flowMonitor runs the "ovs-ofctl monitor" process of one subscription. The
// process runs behind the OVS wrapper, where killing the child may end only
// the wrapper's client: a process started with "docker exec" keeps running
// in the container. A monitor is therefore ended through its control socket,
// and the child is killed only after that.
type flowMonitor struct {
	wrapper      []string
	bridge       string
	ctl          string
	startTimeout time.Duration
	stopTimeout  time.Duration

	// reaped counts the monitor processes that were not reaped yet and
	// their stop goroutines that have not returned.
	reaped sync.WaitGroup
}

// monitorEnd is how a monitor process ended: the error Wait returned and the
// last line it printed that is not part of the flow monitor protocol.
type monitorEnd struct {
	err       error
	lastWords string
}

// exit asks the monitor listening on the control socket to end. A failure is
// logged at debug and nothing else: with no monitor listening it is the
// normal outcome.
func (m *flowMonitor) exit() {
	ctx, cancel := context.WithTimeout(context.Background(), m.stopTimeout)
	defer cancel()
	cmd := ovsCommandContext(ctx, m.wrapper, "ovs-appctl", "-t", m.ctl, "exit")
	// Bounds the wait for the output pipe, which a descendant of a killed
	// wrapper may hold open.
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Debug("OVS flow monitor exit failed", "ctl", m.ctl, "error", err, "output", strings.TrimSpace(string(out)))
	}
}

// subscribe starts a monitor process on the bridge and returns the channel of
// the deletions it reports, once the process signalled that its request was
// answered. The channel is closed when the process has ended. Closing done
// ends the process.
func (m *flowMonitor) subscribe(done <-chan struct{}) (<-chan flowEvent, error) {
	// The stop goroutine of the previous subscription may still be talking
	// to the control socket; its exit must not reach the monitor started
	// below.
	m.reaped.Wait()
	// Ends a monitor left by an earlier subscription or agent process.
	m.exit()

	ctx, kill := context.WithCancel(context.Background())
	cmd := ovsCommandContext(ctx, m.wrapper, "ovs-ofctl", "--no-names", "--unixctl="+m.ctl,
		"monitor", m.bridge, ovsFlowMonitorSpec)
	stdout, err := cmd.StdoutPipe()
	var stderr io.ReadCloser
	if err == nil {
		stderr, err = cmd.StderrPipe()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		kill()
		return nil, fmt.Errorf("start the OVS flow monitor on %s: %w", m.bridge, err)
	}
	m.reaped.Add(2)

	var (
		events     = make(chan flowEvent)
		ready      = make(chan struct{})
		stdoutDone = make(chan struct{})
		ended      = make(chan monitorEnd, 1)
	)

	// The reply header is the only line the monitor prints on stdout, once
	// its request was answered. It is the readiness signal.
	go func() {
		defer close(stdoutDone)
		r := bufio.NewReader(stdout)
		if _, err := r.ReadString('\n'); err == nil {
			close(ready)
		}
		_, _ = io.Copy(io.Discard, r)
	}()

	// Every event goes to stderr, one line per flow, as do the process's own
	// messages.
	go func() {
		defer close(events)
		var lastWords string
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if ev, ok := parseFlowMonitorLine(line); ok {
				select {
				case events <- ev:
				case <-done:
				}
				continue
			}
			if l := strings.TrimSpace(line); l != "" && !strings.Contains(l, "FLOW_MONITOR") {
				lastWords = l
			}
		}
		if sc.Err() != nil {
			// Nothing reads stderr any more, so a monitor that goes on
			// printing would block. End it, so the subscription ends.
			kill()
		}
		<-stdoutDone
		waitErr := cmd.Wait()
		m.reaped.Done()
		if isClosed(ready) && !isClosed(done) {
			slog.Warn("OVS flow monitor ended", "bridge", m.bridge, "error", waitErr, "output", lastWords)
		}
		ended <- monitorEnd{err: waitErr, lastWords: lastWords}
	}()

	go func() {
		defer m.reaped.Done()
		<-done
		m.exit()
		kill()
		// A descendant of the wrapper may keep the write ends open after
		// the kill. Closing the read ends releases the readers anyway.
		_ = stdout.Close()
		_ = stderr.Close()
	}()

	timeout := time.NewTimer(m.startTimeout)
	defer timeout.Stop()
	select {
	case <-ready:
		return events, nil
	case end := <-ended:
		if isClosed(ready) {
			// The process ended right after its header. The closed
			// channel reports that.
			return events, nil
		}
		waitErr := end.err
		if waitErr == nil {
			waitErr = errFlowMonitorEnded
		}
		return nil, fmt.Errorf("OVS flow monitor on %s ended before it was ready: %w (output: %s)", m.bridge, waitErr, end.lastWords)
	case <-timeout.C:
		kill()
		return nil, fmt.Errorf("OVS flow monitor on %s was not ready within %s", m.bridge, m.startTimeout)
	}
}

// isClosed reports whether ch is closed, without blocking.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
