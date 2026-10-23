package nativeudp

import (
	"bytes"
	"context"
	"encoding/binary"
	"runtime"
	"testing"
	"time"
)

func TestNativeSaturatedBackendDoesNotAllocateDiscardedPayloads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	link := &Link{ctx: ctx, in: make(chan []byte, 1)}
	target := "127.0.0.1:53"
	full := &flow{id: 1, target: target, queue: make(chan []byte, 1)}
	full.queue <- []byte("opening packet")
	e := &Engine{ctx: ctx, link: link, max: 1, flows: map[uint64]*flow{1: full}}
	e.wg.Add(1)
	go e.receive()
	defer func() { cancel(); e.wg.Wait() }()
	packet := make([]byte, maxPayload)
	binary.BigEndian.PutUint64(packet, full.id)
	binary.BigEndian.PutUint16(packet[8:], uint16(len(target)))
	copy(packet[10:], target)
	var dropped uint64
	if got := testing.AllocsPerRun(100, func() {
		dropped++
		link.in <- packet
		for e.drops.Load() < dropped {
			runtime.Gosched()
		}
	}); got != 0 {
		t.Fatalf("discarding a saturated backend packet allocated %.1f objects", got)
	}
	if string(<-full.queue) != "opening packet" {
		t.Fatal("overload replaced the queued opening packet")
	}
	// Once the backend drains, the same flow must accept an independent copy
	// before the receive scratch is overwritten by another datagram.
	link.in <- packet
	select {
	case got := <-full.queue:
		if len(got) != maxPayload-10-len(target) || !bytes.Equal(got, make([]byte, len(got))) {
			t.Fatal("flow did not recover with an intact datagram after saturation")
		}
		next := bytes.Clone(packet)
		copy(next[10+len(target):], bytes.Repeat([]byte{0xcc}, len(got)))
		link.in <- next
		select {
		case newer := <-full.queue:
			if newer[0] != 0xcc || got[0] != 0 {
				t.Fatal("queued datagram retained the receiver's mutable scratch")
			}
		case <-ctx.Done():
			t.Fatal("later packet was blocked after saturation")
		}
	case <-ctx.Done():
		t.Fatal("flow stopped after saturation")
	}
}
