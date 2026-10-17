package transport

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestAdaptiveIdlePoolDropsRetiredSlotsButKeepsClaimedHalfCloses(t *testing.T) {
	a, b := net.Pipe()
	c := watchIdlePool(t.Context(), a)
	b.Close()
	select {
	case <-c.done:
	case <-time.After(time.Second):
		t.Fatal("idle retirement was not observed")
	}
	if c.claim() {
		t.Fatal("retired slot was assigned to a user")
	}
	c.Close()
	a, b = net.Pipe()
	defer b.Close()
	c = watchIdlePool(t.Context(), a)
	defer c.Close()
	if !c.claim() {
		t.Fatal("live slot could not be claimed")
	}
	sent := make(chan error, 1)
	go func() { _, err := b.Write([]byte("first application bytes")); sent <- err }()
	got := make([]byte, len("first application bytes"))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "first application bytes" {
		t.Fatalf("lookahead lost payload: %q %v", got, err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
}
