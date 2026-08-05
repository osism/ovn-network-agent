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

// targetState is one probe's live status and history.
type targetState struct {
	up          bool
	sent        int
	lost        int
	transitions int
	// lastUpAt is when the target most recently went from red to green.
	// The engine dates recovery from it.
	lastUpAt time.Time
	buckets  map[int64]*lossBucket
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
	err := probeOnce(ctx, p.lab, t)
	if ctx.Err() != nil {
		// The run is over; a probe cancelled mid-flight is not loss.
		return
	}
	p.record(t.name, err == nil)
}

func (p *prober) record(name string, up bool) {
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
	changed := st.up != up
	if changed {
		st.up = up
		st.transitions++
		if up {
			st.lastUpAt = now
		}
	}
	p.mu.Unlock()

	if changed {
		p.jrnl.emit(event{Event: evProbeTransition, Probe: name, Up: boolPtr(up)})
	}
}

// allGreen reports whether every target is currently up.
func (p *prober) allGreen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.state {
		if !st.up {
			return false
		}
	}
	return true
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
