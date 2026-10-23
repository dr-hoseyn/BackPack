package transport

import (
	"bytes"
	"testing"
)

func TestFullRawUDPQueueDropsWithoutCopyingThePacket(t *testing.T) {
	queue := make(chan []byte, 1)
	first := []byte("keep the opening packet")
	queue <- first
	packet := make([]byte, 60000)
	if got := testing.AllocsPerRun(100, func() {
		if enqueueUDPPayload(queue, packet) {
			t.Fatal("full queue accepted another packet")
		}
	}); got != 0 {
		t.Fatalf("discarding a packet allocated %.1f objects", got)
	}
	if !bytes.Equal(<-queue, first) {
		t.Fatal("discard replaced the opening packet")
	}
	if !enqueueUDPPayload(queue, packet) {
		t.Fatal("drained queue did not accept traffic again")
	}
	packet[0] = 1
	if (<-queue)[0] != 0 {
		t.Fatal("queue retained the listener's mutable read buffer")
	}
	if !enqueueUDPPayload(queue, nil) || len(<-queue) != 0 {
		t.Fatal("empty datagram was lost")
	}
}
