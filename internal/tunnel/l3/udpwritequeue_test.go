package l3

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/tunnel/limits"
)

func TestUDPWriteQueueServesAffordableFlowBeforeWaitingForBulk(t *testing.T) {
	q := newUDPWriteQueue()
	bulk := &udpFlow{ctx: context.Background()}
	light := &udpFlow{ctx: context.Background()}
	q.enqueue(bulk, bytes.Repeat([]byte("b"), 60000))
	q.enqueue(light, []byte("call"))
	q.enqueue(light, []byte("next"))
	credit := 8
	take := func(n int) time.Duration {
		if n > credit {
			return time.Duration(n-credit) * time.Microsecond
		}
		credit -= n
		return 0
	}
	for _, want := range []string{"call", "next"} {
		flow, packet, delay := q.turnWithBudget(take)
		if flow != light || string(packet) != want || delay != 0 {
			t.Fatalf("small flow waited behind bulk: flow=%p packet=%q delay=%s", flow, packet, delay)
		}
	}
	if flow, _, delay := q.turnWithBudget(take); flow != nil || delay <= 0 {
		t.Fatal("exhausted budget did not pause bulk")
	}
	credit = 60000
	flow, packet, delay := q.turnWithBudget(take)
	if flow != bulk || len(packet) != 60000 || delay != 0 || credit != 0 {
		t.Fatal("bulk did not resume intact within its budget")
	}
}

func TestUDPWriteQueueRetriesTheEarliestAffordableDatagram(t *testing.T) {
	q := newUDPWriteQueue()
	for _, size := range []int{1200, 10, 40} {
		q.enqueue(&udpFlow{ctx: context.Background()}, make([]byte, size))
	}
	flow, _, delay := q.turnWithBudget(func(n int) time.Duration { return time.Duration(n) * time.Millisecond })
	if flow != nil || delay != 10*time.Millisecond || q.packets != 3 {
		t.Fatalf("wrong retry budget or dropped packet: delay=%s packets=%d", delay, q.packets)
	}
}

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
