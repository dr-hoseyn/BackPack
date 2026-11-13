package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// wssMuxDemand counts requested capacity while TLS handshakes are in flight.
// A control request creates a physical session, not one stream. Without these
// credits, a cold burst asks for one TLS session per queued user.
// Credits are estimates, not a wire acknowledgement: the first sessions may
// belong to the client's initial pool. Expiring sent credits once per second
// retries failed/lost requests without depending on another user arriving.
type wssMuxDemand struct {
	ctx      context.Context
	mu       *sync.Mutex // the transport's generation counter lock
	streams  *int32
	sessions *int32
	muxCon   int
	requests chan struct{}
	pending  int64
	retryAt  time.Time
}

func (d *wssMuxDemand) queued() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	atomic.AddInt32(d.streams, 1)
	d.reconcile()
}

func (d *wssMuxDemand) arrived() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	if d.pending > 0 {
		if d.pending == int64(len(d.requests)) {
			// No sent credit exists yet: an initial-pool arrival replaces
			// one request still waiting for control to consume it.
			select {
			case <-d.requests:
			default:
			}
		}
		d.pending--
	}
	d.reconcile()
}

func (d *wssMuxDemand) run() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-tick.C:
			d.mu.Lock()
			if d.ctx.Err() == nil {
				d.reconcile()
			}
			d.mu.Unlock()
		}
	}
}

// Caller holds mu. Keep the old one-session/25% reserve and the same fast
// path when live capacity is already sufficient. Only capacity acquisition
// changes; stream admission, carrier selection and retirement do not.
func (d *wssMuxDemand) reconcile() {
	n := int64(atomic.LoadInt32(d.streams))
	live := int64(atomic.LoadInt32(d.sessions))
	perSession := int64(max(d.muxCon, 1))
	capacity := live * perSession
	if n <= 0 || n < capacity-max(capacity/4, perSession) {
		if d.pending == 0 {
			return
		}
		d.pending = 0
		d.retryAt = time.Time{}
		// Requests still sitting in the control queue have no owner yet.
		// Do not let obsolete demand create sessions after users drain.
		for {
			select {
			case <-d.requests:
			default:
				return
			}
		}
	}
	now := time.Now()
	if d.pending > 0 && !now.Before(d.retryAt) {
		// Retain credits for requests that control has not consumed. An
		// absent client must not accumulate a fresh batch every tick.
		d.pending = int64(len(d.requests))
		d.retryAt = now.Add(time.Second)
	}
	// Each bound leaves strictly more than n stream slots after reserving
	// either one session or one quarter of capacity (as in muxRequest).
	target := max(n/perSession+2, 4*n/(3*perSession)+1)
	for missing := target - live - d.pending; missing > 0; missing-- {
		select {
		case d.requests <- struct{}{}:
			if d.pending == 0 {
				d.retryAt = now.Add(time.Second)
			}
			d.pending++
		default:
			return
		}
	}
}
