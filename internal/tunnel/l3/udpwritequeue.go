package l3

import (
	"bytes"
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/backpack/backpack/internal/tunnel/limits"
	"github.com/sirupsen/logrus"
)

const (
	udpQueuePerFlow = 16
	udpQueuePackets = 1024
	udpQueueBytes   = 4 << 20
	// Charge a large datagram over multiple turns, then send it intact.
	udpPaceQuantum = 1200
	udpWriteWait   = 10 * time.Millisecond
)

type udpWriteQueue struct {
	mu      sync.Mutex
	active  list.List
	byFlow  map[*udpFlow]*list.Element
	packets int
	bytes   int
	wake    chan struct{}
}

type udpPendingFlow struct {
	flow    *udpFlow
	queue   [][]byte
	charged int // bytes charged for the current head datagram
}

func newUDPWriteQueue() *udpWriteQueue {
	return &udpWriteQueue{byFlow: make(map[*udpFlow]*list.Element), wake: make(chan struct{}, 1)}
}

// enqueue drops newest on overflow. No queued datagram references the receive
// buffer, and only flows with pending datagrams have entries here.
func (q *udpWriteQueue) enqueue(flow *udpFlow, p []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.packets >= udpQueuePackets || len(p) > udpQueueBytes-q.bytes || flow.ctx.Err() != nil {
		return false
	}
	elem := q.byFlow[flow]
	if elem == nil {
		elem = q.active.PushBack(&udpPendingFlow{flow: flow})
		q.byFlow[flow] = elem
	}
	pending := elem.Value.(*udpPendingFlow)
	if len(pending.queue) >= udpQueuePerFlow {
		return false
	}
	pending.queue = append(pending.queue, bytes.Clone(p))
	q.packets++
	q.bytes += len(p)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// turn charges at most a quantum to one flow, preserving each flow's FIFO.
// Socket writes happen outside the lock so ingress never waits on pacing or I/O.
func (q *udpWriteQueue) turn(limiter *limits.Limiter) (*udpFlow, []byte, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.active.Len() != 0 {
		elem := q.active.Front()
		pending := elem.Value.(*udpPendingFlow)
		if pending.flow.ctx.Err() != nil {
			for _, packet := range pending.queue {
				q.bytes -= len(packet)
			}
			q.packets -= len(pending.queue)
			delete(q.byFlow, pending.flow)
			q.active.Remove(elem)
			continue
		}
		packet := pending.queue[0]
		chunk := min(udpPaceQuantum, len(packet)-pending.charged)
		if delay := limiter.TryTakeBytes(chunk); delay > 0 {
			return nil, nil, delay
		}
		pending.charged += chunk
		q.active.MoveToBack(elem)
		if pending.charged != len(packet) {
			continue
		}
		pending.charged = 0
		pending.queue[0] = nil
		pending.queue = pending.queue[1:]
		q.packets--
		q.bytes -= len(packet)
		if len(pending.queue) == 0 {
			delete(q.byFlow, pending.flow)
			q.active.Remove(elem)
		}
		return pending.flow, packet, 0
	}
	return nil, nil, 0
}

func (q *udpWriteQueue) run(ctx context.Context, limiter *limits.Limiter, log *logrus.Logger) {
	for ctx.Err() == nil {
		flow, packet, delay := q.turn(limiter)
		if flow != nil {
			_ = flow.socket.SetWriteDeadline(time.Now().Add(udpWriteWait))
			if _, err := flow.socket.Write(packet); err != nil && flow.ctx.Err() == nil {
				log.Debugf("l3: forwarding udp to %s: %v", flow.socket.RemoteAddr(), err)
			}
			continue
		}
		if delay > 0 {
			timer := time.NewTimer(min(delay, udpWriteWait))
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
			timer.Stop()
			continue
		}
		select {
		case <-ctx.Done():
		case <-q.wake:
		}
	}
}
