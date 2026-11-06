package l3

import (
	"context"
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
