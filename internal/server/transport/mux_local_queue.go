package transport

import (
	"container/list"
	"context"
	"sync"
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
	waiters  map[*muxQueueWaiter]struct{}
	selected *muxQueueWaiter
	lastUse  uint64
	warm     int
}

type muxQueueWaiter struct {
	ready   chan struct{}
	slots   chan struct{}
	lastUse uint64
	warm    bool
}

const (
	num3WarmCarriers = 2
	num3SpareLoad    = 4
)

func newMuxLocalQueue(limit int) *muxLocalQueue {
	return &muxLocalQueue{limit: limit, ready: make(chan struct{}, 1)}
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
		if q.selected != nil || q.closed || q.entries.Len() == 0 {
			return
		}
		var bestLoad int
		for waiter := range q.waiters {
			q.warmCarrier(waiter)
			// The reserved slot is present on every eligible worker.
			// Give the warm pair the first few streams, then use the
			// rest of the pool as concurrent load builds.
			load := len(waiter.slots) - 1
			if !waiter.warm {
				load += num3SpareLoad
			}
			if q.selected == nil || load < bestLoad ||
				(load == bestLoad && waiter.lastUse < q.selected.lastUse) {
				q.selected, bestLoad = waiter, load
			}
		}
		if q.selected != nil {
			q.selected.ready <- struct{}{}
		}
		return
	}
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *muxLocalQueue) warmCarrier(waiter *muxQueueWaiter) {
	if !waiter.warm && q.warm < num3WarmCarriers {
		waiter.warm = true
		q.warm++
	}
}

func (q *muxLocalQueue) wait(waiter *muxQueueWaiter) <-chan struct{} {
	if waiter == nil {
		return q.ready
	}
	q.mu.Lock()
	q.warmCarrier(waiter)
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
			q.warm--
			waiter.warm = false
		}
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
	}
	return local, true
}

func (q *muxLocalQueue) drain(ctx context.Context, limits *limiter) {
	<-ctx.Done()
	q.mu.Lock()
	q.closed = true
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
