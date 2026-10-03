package main

import (
	"context"
	"slices"
	"sync"
	"time"
)

const (
	probeInterval   = time.Second
	lossBucketWidth = 10 * time.Second

	// confirmGreenSamples is how many consecutive green samples, all started
	// after a fault's restore, confirm a probe green again. One is not
	// enough: the drift faults lose the first probe sent after the restore,
	// and a returning chassis loses one sent up to 2 s after it (#292).
	confirmGreenSamples = 2

	// confirmationTime bounds how long a probe confirmation takes after a
	// restore: up to one probeInterval until the first sample started after
	// it, plus one probeInterval per confirming sample.
	confirmationTime = (confirmGreenSamples + 1) * probeInterval
)

type probeKind int

const (
	probePing probeKind = iota
	probeHTTP
	// probeTCP completes a TCP handshake and nothing more. It is what a
	// vantage inside the lab can do without curl, which the gateway image
	// does not carry.
	probeTCP
)

// probeTarget is one continuously-measured reachability check.
//
// The zero vantage — empty node and netns — is client-1's default
// namespace, the external vantage every scenario probes from, so a target
// that names neither is measured exactly as it was before vantages
// existed. Naming a node and a netns instead measures a path from inside
// the lab: same-chassis FIP-to-FIP and FIP-to-VIP traffic never leaves
// the chassis, so client-1 cannot see it at all.
type probeTarget struct {
	name  string
	kind  probeKind
	addr  string
	node  string
	netns string
}

// probeOnce runs one reachability check from its target's vantage. It is
// the single dispatch both the continuous prober and the start-state gate
// go through, so a target neither of them can measure is impossible to
// add.
func probeOnce(ctx context.Context, l *lab, t probeTarget) error {
	switch t.kind {
	case probeHTTP:
		return l.httpGet(ctx, t.addr)
	case probeTCP:
		return l.tcpConnectFrom(ctx, t.node, t.netns, t.addr)
	default:
		return l.pingFrom(ctx, t.node, t.netns, t.addr)
	}
}

// lossWindow is one contiguous red span of a target. end stays zero
// while the target is still down.
type lossWindow struct {
	start, end time.Time
}

// targetState is one probe's live status and history.
type targetState struct {
	up          bool
	sent        int
	lost        int
	transitions int
	// lastUpAt is when the target most recently went from red to green.
	// It dates the legacy from_inject_ms; windows is what down_ms sums.
	lastUpAt time.Time
	// windows is every red span since the run started, in order; the
	// engine sums the ones after an anchor.
	windows []lossWindow
	buckets map[int64]*lossBucket
	// greenStarts is when each of the consecutive green samples that end
	// the target's history started, oldest first and never more than
	// confirmGreenSamples entries. A red sample empties it.
	greenStarts []time.Time
}

// prober measures every probe target continuously — one goroutine per
// target — for the whole run, so loss during a fault hold is recorded
// even though the engine is blocked executing the action.
type prober struct {
	lab       *lab
	targets   []probeTarget
	jrnl      *journal
	now       func() time.Time
	startedAt time.Time

	mu    sync.Mutex
	state map[string]*targetState
	wg    sync.WaitGroup
}

func newProber(l *lab, targets []probeTarget, jrnl *journal, now func() time.Time) *prober {
	p := &prober{
		lab:       l,
		targets:   targets,
		jrnl:      jrnl,
		now:       now,
		startedAt: now(),
		state:     make(map[string]*targetState, len(targets)),
	}
	for _, t := range targets {
		// Start green: a target that is already down when the run
		// begins produces a transition on its first sample, which is
		// exactly what the operator wants to see in the journal.
		p.state[t.name] = &targetState{up: true, buckets: map[int64]*lossBucket{}}
	}
	return p
}

// start launches one sampling goroutine per target and returns. They
// exit when ctx is cancelled; stop() waits for them. Unlike engine.run
// and baselineChecks.run, this one does not block.
func (p *prober) start(ctx context.Context) {
	for _, t := range p.targets {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			ticker := time.NewTicker(probeInterval)
			defer ticker.Stop()
			for {
				p.sample(ctx, t)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
}

func (p *prober) stop() { p.wg.Wait() }

// sample runs one probe and folds the result into the target's state.
//
// A `docker exec` that could not run at all — the node is down, the netns
// went with it mid-fault — is loss, not an error to report: the path
// really is down, which is what the probe measures.
func (p *prober) sample(ctx context.Context, t probeTarget) {
	started := p.now()
	err := probeOnce(ctx, p.lab, t)
	if ctx.Err() != nil {
		// The run is over; a probe cancelled mid-flight is not loss.
		return
	}
	p.record(t.name, err == nil, started)
}

// record folds one sample into the target's state; started is when its
// probe was sent.
func (p *prober) record(name string, up bool, started time.Time) {
	now := p.now()

	p.mu.Lock()
	st, ok := p.state[name]
	if !ok {
		p.mu.Unlock()
		return
	}
	st.sent++
	if !up {
		st.lost++
	}
	offset := now.Sub(p.startedAt) / lossBucketWidth * lossBucketWidth
	bucket, ok := st.buckets[offset.Milliseconds()]
	if !ok {
		bucket = &lossBucket{OffsetMS: offset.Milliseconds()}
		st.buckets[offset.Milliseconds()] = bucket
	}
	bucket.Sent++
	if !up {
		bucket.Lost++
	}
	if up {
		st.greenStarts = append(st.greenStarts, started)
		if n := len(st.greenStarts); n > confirmGreenSamples {
			st.greenStarts = slices.Delete(st.greenStarts, 0, n-confirmGreenSamples)
		}
	} else {
		st.greenStarts = st.greenStarts[:0]
	}
	changed := st.up != up
	if changed {
		st.up = up
		st.transitions++
		// The edges alternate: changed is only true when the state flips,
		// and every target starts green, so on an up edge the last window
		// is the open one.
		if up {
			st.lastUpAt = now
			st.windows[len(st.windows)-1].end = now
		} else {
			st.windows = append(st.windows, lossWindow{start: now})
		}
	}
	p.mu.Unlock()

	if changed {
		p.jrnl.emit(event{Event: evProbeTransition, Probe: name, Up: boolPtr(up)})
	}
}

// redTargets names the targets that are currently down — the detail a
// recovery-timeout violation carries.
func (p *prober) redTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var red []string
	for _, t := range p.targets {
		if st := p.state[t.name]; st != nil && !st.up {
			red = append(red, t.name)
		}
	}
	return red
}

// unconfirmedSince names the targets not confirmed green since anchor, in
// target order. A target is confirmed once its last confirmGreenSamples
// samples were all green and all started at or after anchor.
func (p *prober) unconfirmedSince(anchor time.Time) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var unconfirmed []string
	for _, t := range p.targets {
		st := p.state[t.name]
		if st == nil {
			continue
		}
		if len(st.greenStarts) == confirmGreenSamples && !st.greenStarts[0].Before(anchor) {
			continue
		}
		unconfirmed = append(unconfirmed, t.name)
	}
	return unconfirmed
}

// recoverySince reports, per target, how long after `anchor` it came
// back. A target that never went red after the anchor recovers in 0 ms.
func (p *prober) recoverySince(anchor time.Time) map[string]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int64, len(p.targets))
	for _, t := range p.targets {
		st := p.state[t.name]
		if st == nil {
			continue
		}
		if st.lastUpAt.After(anchor) {
			out[t.name] = st.lastUpAt.Sub(anchor).Milliseconds()
			continue
		}
		out[t.name] = 0
	}
	return out
}

// downtimeSince reports, per target, how long it was red after `anchor`
// and in how many separate windows. A window that was already open at
// the anchor counts from the anchor; a window still open now counts to
// now. A target with no red span after the anchor reports 0 and 0.
func (p *prober) downtimeSince(anchor time.Time) (ms map[string]int64, windows map[string]int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	ms = make(map[string]int64, len(p.targets))
	windows = make(map[string]int, len(p.targets))
	for _, t := range p.targets {
		st := p.state[t.name]
		if st == nil {
			continue
		}
		ms[t.name] = 0
		windows[t.name] = 0
		for _, w := range st.windows {
			end := w.end
			if end.IsZero() {
				end = now
			}
			if !end.After(anchor) {
				continue
			}
			start := w.start
			if start.Before(anchor) {
				start = anchor
			}
			ms[t.name] += end.Sub(start).Milliseconds()
			windows[t.name]++
		}
	}
	return ms, windows
}

// summary renders the per-target run-record section, buckets ordered by
// offset so "probe loss over time" reads as a series.
func (p *prober) summary() map[string]probeSummary {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]probeSummary, len(p.targets))
	for _, t := range p.targets {
		st := p.state[t.name]
		if st == nil {
			continue
		}
		sum := probeSummary{
			Target:      t.addr,
			Sent:        st.sent,
			Lost:        st.lost,
			Transitions: st.transitions,
			Buckets:     []lossBucket{},
		}
		offsets := make([]int64, 0, len(st.buckets))
		for offset := range st.buckets {
			offsets = append(offsets, offset)
		}
		slices.Sort(offsets)
		for _, offset := range offsets {
			sum.Buckets = append(sum.Buckets, *st.buckets[offset])
		}
		out[t.name] = sum
	}
	return out
}
