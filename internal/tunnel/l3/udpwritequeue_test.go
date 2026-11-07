package l3

import (
	"context"
	"net"
	"testing"

	"github.com/backpack/backpack/internal/tunnel/limits"
)

func TestUDPWriteQueueBoundsMemoryAndReleasesClosedFlows(t *testing.T) {
	q := newUDPWriteQueue()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	flow := &udpFlow{ctx: ctx}
	for i := 0; i < udpQueuePerFlow; i++ {
		if !q.enqueue(flow, nil) {
			t.Fatal("queue filled early")
		}
	}
	if q.enqueue(flow, nil) {
		t.Fatal("flow packet cap exceeded")
	}
	cancel()
	limiter := limits.New(limits.Config{BandwidthMbps: 1})
	if next, _, _ := q.turn(limiter); next != nil || q.packets != 0 || len(q.byFlow) != 0 {
		t.Fatal("closed flow retained its queue")
	}
	payload := make([]byte, udpBufferSize)
	for i := 0; i < udpQueueBytes/len(payload); i++ {
		if !q.enqueue(&udpFlow{ctx: context.Background()}, payload) {
			t.Fatal("byte cap triggered early")
		}
	}
	if q.enqueue(&udpFlow{ctx: context.Background()}, payload) {
		t.Fatal("global byte cap exceeded")
	}
}

func TestUDPFlowCloseFreesQueueWhileAnotherFlowWaits(t *testing.T) {
	q := newUDPWriteQueue()
	bulk := &udpFlow{ctx: context.Background()}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closing := &udpFlow{ctx: ctx, cancel: cancel, backend: a, pacer: q}
	q.enqueue(bulk, make([]byte, 1200))
	q.enqueue(closing, []byte("queued"))
	closing.close()
	if q.packets != 1 || q.bytes != 1200 || q.byFlow[closing] != nil {
		t.Fatal("closed flow retained queued datagrams")
	}
	if q.enqueue(closing, []byte("late")) {
		t.Fatal("closed flow reentered the queue")
	}
}
