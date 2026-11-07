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
}

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
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *muxLocalQueue) pop() (LocalTCPConn, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := q.entries.Front()
	if entry == nil {
		return LocalTCPConn{}, false
	}
	local := q.entries.Remove(entry).(LocalTCPConn)
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
