package masqueh2

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestChangingWriteDeadlineInterruptsAnAlreadyBlockedCapsule(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newConn(t.Context())
	defer c.Close()
	s := newSession(c.ctx, a, false, nil, nil, nil)
	s.connWindow = 0
	s.streamWindow = 0
	c.sessions[b.LocalAddr().String()] = s
	done := make(chan error, 1)
	go func() { _, err := c.WriteTo([]byte("blocked packet"), b.LocalAddr()); done <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		active := len(c.writes) > 0
		c.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("write never began")
		}
		time.Sleep(time.Millisecond)
	}
	c.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("deadline error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending write ignored updated deadline")
	}
}

func TestCancelledCapsuleCannotWaitBehindAnotherWriterForever(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s := newSession(t.Context(), a, false, nil, nil, nil)
	defer s.close()
	s.sendGate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.send(ctx, []byte("queued")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation: %v", err)
	}
	<-s.sendGate
}
