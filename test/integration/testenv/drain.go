//go:build integration

package testenv

import (
	"context"
	"testing"
	"time"

	"github.com/ovn-kubernetes/libovsdb/client"
)

// DrainRebindOpts configures StartDrainRebind, the shared "simulate the
// takeover peer" goroutine that the drain scenarios drive. Once the watched
// Gateway_Chassis row reaches priority 0 (the leaving agent has lowered its
// own priority), the goroutine rebinds the chassisredirect Port_Binding to a
// peer so the leaving node's countLocalCRPorts drops to 0, then optionally
// stamps the takeover readiness marker so the leaving node's settle releases
// on the marker instead of at drain_timeout.
type DrainRebindOpts struct {
	// GatewayChassisName is the NB Gateway_Chassis row (name) whose priority
	// the goroutine watches; it acts once that row reaches priority 0.
	GatewayChassisName string
	// CRPortUUID is the SB chassisredirect Port_Binding that is rebound to the
	// peer.
	CRPortUUID string
	// PeerChassisUUID is the SB Chassis the CR port is rebound to.
	PeerChassisUUID string

	// MarkerRouteUUID, when non-empty, is the NB Logical_Router_Static_Route
	// the readiness marker (MarkerExternalIDs) is stamped onto after the
	// rebind. Empty leaves the marker unwritten — the timeout-fallback
	// scenario, where the leaving node must settle at drain_timeout instead.
	MarkerRouteUUID   string
	MarkerExternalIDs map[string]string
}

// DrainRebind is a handle to a running StartDrainRebind goroutine.
type DrainRebind struct {
	// PriorityZeroAt receives the moment the goroutine first observed the
	// watched Gateway_Chassis at priority 0, sent before the rebind, so a
	// caller can assert priority-0 was observed before route removal.
	// Buffered (cap 1); callers that do not need the timestamp ignore it.
	PriorityZeroAt <-chan time.Time

	cancel context.CancelFunc
	errCh  chan error
	done   chan struct{}
}

// StartDrainRebind spawns the takeover-peer goroutine and returns immediately.
// The goroutine polls NB every 50ms until opts.GatewayChassisName shows
// priority 0, then performs the rebind (and optional marker stamp) exactly
// once and exits. testing.T methods are never called from the goroutine —
// errors are surfaced via the handle's Finish, which the caller must invoke.
func StartDrainRebind(t *testing.T, ctx context.Context, nb, sb client.Client, opts DrainRebindOpts) *DrainRebind {
	t.Helper()

	priorityZeroAt := make(chan time.Time, 1)
	errCh := make(chan error, 1)
	done := make(chan struct{})
	pctx, pcancel := context.WithCancel(ctx)

	go func() {
		defer close(done)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-tick.C:
				var entries []NBGatewayChassis
				if err := nb.List(ctx, &entries); err != nil {
					sendErr(errCh, err)
					return
				}
				for _, gc := range entries {
					if gc.Name != opts.GatewayChassisName || gc.Priority != 0 {
						continue
					}
					// Record the observation before rebinding so an ordering
					// assertion (priority_zero < route_removed) has signal.
					select {
					case priorityZeroAt <- time.Now():
					default:
					}
					peer := opts.PeerChassisUUID
					rebind := &SBPortBinding{UUID: opts.CRPortUUID, Chassis: &peer}
					ops, err := sb.Where(rebind).Update(rebind, &rebind.Chassis)
					if err != nil {
						sendErr(errCh, err)
						return
					}
					if _, err := sb.Transact(ctx, ops...); err != nil {
						sendErr(errCh, err)
						return
					}
					if opts.MarkerRouteUUID != "" {
						mark := &NBLogicalRouterStaticRoute{UUID: opts.MarkerRouteUUID, ExternalIDs: opts.MarkerExternalIDs}
						mops, err := nb.Where(mark).Update(mark, &mark.ExternalIDs)
						if err != nil {
							sendErr(errCh, err)
							return
						}
						if _, err := nb.Transact(ctx, mops...); err != nil {
							sendErr(errCh, err)
						}
					}
					return
				}
			}
		}
	}()

	return &DrainRebind{
		PriorityZeroAt: priorityZeroAt,
		cancel:         pcancel,
		errCh:          errCh,
		done:           done,
	}
}

// Finish stops the goroutine, waits for it to exit, and fails the test if it
// recorded an OVSDB error. Waiting for the goroutine to exit before reading
// errCh closes the race the inlined copies had, where a cancel followed by a
// non-blocking read could miss an in-flight transaction error.
func (d *DrainRebind) Finish(t *testing.T) {
	t.Helper()
	d.cancel()
	<-d.done
	select {
	case err := <-d.errCh:
		t.Fatalf("drain-rebind helper: %v", err)
	default:
	}
}

// sendErr performs a non-blocking send so the goroutine never deadlocks on a
// full (cap-1) error channel — the first error is enough to fail the test.
func sendErr(ch chan error, err error) {
	select {
	case ch <- err:
	default:
	}
}
