package transport

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// muxLocalQueue has the same capacity and FIFO order as the local channel,
// with the additional ability to remove an expired request in place.
type muxLocalQueue struct {
	mu      sync.Mutex
	entries list.List
	limit   int
	closed  bool
	ready   chan struct{}
	// TCP Forward keeps a small group of carriers active before using spares.
	waiters map[*muxQueueWaiter]struct{}
	// WebSocket mux spreads new users over healthy carriers; it must not
	// inherit TCP Forward's preference for concentrating work on a warm pair.
	balanced   bool
	selected   *muxQueueWaiter
	lastUse    uint64
	warm       [num3WarmCarriers]*muxQueueWaiter
	warmTimer  *time.Timer
	warmWaited bool
}

type muxQueueWaiter struct {
	ready    chan struct{}
	slots    chan struct{}
	done     <-chan struct{}
	lastUse  uint64
	warm     bool
	pressure func() int
}

func (w *muxQueueWaiter) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

const (
	num3WarmCarriers = 2
	num3SpareLoad    = 16
	num3SpareReuse   = 2
	num3WarmWait     = 2 * time.Millisecond
)

func newMuxLocalQueue(limit int) *muxLocalQueue {
	return &muxLocalQueue{limit: limit, ready: make(chan struct{}, 1)}
}

func newBalancedMuxLocalQueue(limit int) *muxLocalQueue {
	q := newMuxLocalQueue(limit)
	q.waiters = make(map[*muxQueueWaiter]struct{})
	q.balanced = true
	return q
}

func (q *muxLocalQueue) push(local LocalTCPConn, queued func()) bool {
	p := local.pairing
	if p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
	}
	q.mu.Lock()
	if q.closed || q.entries.Len() >= q.limit {
		q.mu.Unlock()
		return false
	}
	entry := q.entries.PushBack(local)
	q.notify()
	q.mu.Unlock()

	if queued != nil {
		queued()
	}
	if p != nil {
		p.remove = func() {
			q.mu.Lock()
			q.entries.Remove(entry)
			if q.waiters != nil && q.entries.Len() == 0 {
				q.clearWarmWait()
			}
			q.mu.Unlock()
		}
		p.arm(local.timeCreated)
	}
	return true
}

// notify wakes one worker. A pop passes the wake to another worker when
// requests remain, instead of waking every session for a single connection.
func (q *muxLocalQueue) notify() {
	if q.waiters != nil {
		// Socket observation on a closed carrier can report zero pressure.
		// Do not interpret that as a healthy path while its worker is still
		// waking up to leave the queue.
		if q.selected != nil && q.selected.isClosed() {
			q.removeWaiter(q.selected)
		}
		if q.selected != nil || q.closed || q.entries.Len() == 0 {
			return
		}
		if q.balanced {
			if next := q.uncongestedLeastBusy(); next != nil {
				q.selected = next
				next.ready <- struct{}{}
				return
			}
		}
		var bestLoad, bestPressure int
		var next *muxQueueWaiter
		for waiter := range q.waiters {
			if waiter.isClosed() {
				continue
			}
			if !q.balanced {
				q.warmCarrier(waiter)
			}
			// The reserved slot is present on every eligible worker.
			// Give the warm pair the first few streams, then use the
			// rest of the pool as concurrent load builds.
			load := len(waiter.slots) - 1
			pressure := 0
			if waiter.pressure != nil {
				pressure = waiter.pressure()
			}
			if !q.balanced {
				load += pressure
				pressure = 0
			}
			if !q.balanced && !waiter.warm {
				load += num3SpareLoad
				if waiter.lastUse != 0 {
					// Reuse spares that already carried work before
					// spreading a burst onto more fresh TCP sockets.
					load -= num3SpareReuse
				}
			}
			// On WS/WSS, an idle stream count cannot make a retransmission-
			// stalled socket healthier than a busy but draining one. Compare
			// congestion first, then occupancy, rotating equal candidates.
			if next == nil || pressure < bestPressure || (pressure == bestPressure &&
				(load < bestLoad || (load == bestLoad && waiter.lastUse < next.lastUse))) {
				next, bestLoad, bestPressure = waiter, load, pressure
			}
		}
		if next != nil && (q.balanced || next.warm || !q.waitForWarm(bestLoad)) {
			q.selected = next
			next.ready <- struct{}{}
		}
		return
	}
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// Pressure is nonnegative. If the least occupied, least recently used carrier
// has zero pressure, no other carrier can beat it. Sample that candidate first
// instead of reading every TCP socket on the healthy admission path. A busy
// candidate falls back to the full congestion-first search in notify.
// The queue mutex protects waiter membership and recency, as in the full scan.
func (q *muxLocalQueue) uncongestedLeastBusy() *muxQueueWaiter {
	var next *muxQueueWaiter
	var bestLoad int
	for waiter := range q.waiters {
		if waiter.isClosed() {
			continue
		}
		load := len(waiter.slots) - 1
		if next == nil || load < bestLoad || (load == bestLoad && waiter.lastUse < next.lastUse) {
			next, bestLoad = waiter, load
		}
	}
	if next != nil && (next.pressure == nil || next.pressure() == 0) {
		return next
	}
	return nil
}

func (q *muxLocalQueue) warmCarrier(waiter *muxQueueWaiter) {
	if waiter.warm {
		return
	}
	for i, warm := range q.warm {
		if warm == nil {
			q.warm[i] = waiter
			waiter.warm = true
			return
		}
	}
}

// Opening a stream briefly takes its carrier out of the waiting set. Let
// a warm carrier with room return before using a spare. A busy queue gets
// at most one grace period, even if the opener is stuck.
func (q *muxLocalQueue) waitForWarm(spareLoad int) bool {
	if q.warmWaited && q.warmTimer == nil {
		return false
	}
	for _, warm := range q.warm {
		if warm == nil || warm.isClosed() || len(warm.slots) >= min(cap(warm.slots), spareLoad) {
			continue
		}
		if warm.pressure != nil && len(warm.slots)+warm.pressure() >= spareLoad {
			continue
		}
		if _, waiting := q.waiters[warm]; waiting {
			continue
		}
		if q.warmTimer == nil {
			q.warmWaited = true
			var timer *time.Timer
			timer = time.AfterFunc(num3WarmWait, func() {
				q.mu.Lock()
				defer q.mu.Unlock()
				if q.warmTimer == timer {
					q.warmTimer = nil
					q.notify()
				}
			})
			q.warmTimer = timer
		}
		return true
	}
	return false
}

func (q *muxLocalQueue) clearWarmWait() {
	if q.warmTimer != nil {
		q.warmTimer.Stop()
		q.warmTimer = nil
	}
	q.warmWaited = false
}

func (q *muxLocalQueue) wait(waiter *muxQueueWaiter) <-chan struct{} {
	if waiter == nil {
		return q.ready
	}
	q.mu.Lock()
	if waiter.isClosed() {
		q.removeWaiter(waiter)
		q.notify()
		q.mu.Unlock()
		return waiter.ready
	}
	if !q.balanced {
		q.warmCarrier(waiter)
	}
	q.waiters[waiter] = struct{}{}
	q.notify()
	q.mu.Unlock()
	return waiter.ready
}

func (q *muxLocalQueue) removeWaiter(waiter *muxQueueWaiter) {
	delete(q.waiters, waiter)
	if q.selected == waiter {
		q.selected = nil
		select {
		case <-waiter.ready:
		default:
		}
	}
}

func (q *muxLocalQueue) leave(waiter *muxQueueWaiter) {
	if waiter != nil {
		q.mu.Lock()
		q.removeWaiter(waiter)
		if waiter.warm {
			for i, warm := range q.warm {
				if warm == waiter {
					q.warm[i] = nil
					break
				}
			}
			waiter.warm = false
		}
		q.notify()
		q.mu.Unlock()
	}
}

// Pause a wait for an idle check while retaining the carrier's warm slot.
func (q *muxLocalQueue) unwait(waiter *muxQueueWaiter) {
	if waiter != nil {
		q.mu.Lock()
		q.removeWaiter(waiter)
		q.notify()
		q.mu.Unlock()
	}
}

func (q *muxLocalQueue) pop() (LocalTCPConn, bool) {
	return q.popFor(nil)
}

func (q *muxLocalQueue) popFor(waiter *muxQueueWaiter) (LocalTCPConn, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if waiter != nil {
		q.removeWaiter(waiter)
		// A worker may already have consumed its notification when the
		// carrier closes. Leave the user queued for a live replacement.
		if waiter.isClosed() {
			q.notify()
			return LocalTCPConn{}, false
		}
	}
	entry := q.entries.Front()
	if entry == nil {
		return LocalTCPConn{}, false
	}
	local := q.entries.Remove(entry).(LocalTCPConn)
	if waiter != nil {
		q.lastUse++
		waiter.lastUse = q.lastUse
	}
	if q.entries.Len() != 0 {
		q.notify()
	} else if q.waiters != nil {
		q.clearWarmWait()
	}
	return local, true
}

func (q *muxLocalQueue) drain(ctx context.Context, limits *limiter) {
	<-ctx.Done()
	q.mu.Lock()
	q.closed = true
	q.clearWarmWait()
	q.mu.Unlock()
	for {
		local, ok := q.pop()
		if !ok {
			return
		}
		if local.pairing.claim() {
			if local.conn != nil {
				local.conn.Close()
			}
			limits.release()
		}
	}
}
