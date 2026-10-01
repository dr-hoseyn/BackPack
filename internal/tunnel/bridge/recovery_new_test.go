package bridge

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type recoveryConn struct {
	net.Conn
	closed   chan struct{}
	reading  chan struct{}
	released <-chan struct{}
	once     sync.Once
}

func (c *recoveryConn) Read([]byte) (int, error) {
	close(c.reading)
	<-c.closed
	<-c.released
	return 0, io.EOF
}
func (c *recoveryConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *recoveryConn) Close() error                { c.once.Do(func() { close(c.closed) }); return nil }

func TestRecoveryNewCancellationWaitsForBothCopies(t *testing.T) {
	fast, slow := make(chan struct{}), make(chan struct{})
	close(fast)
	a := &recoveryConn{closed: make(chan struct{}), reading: make(chan struct{}), released: fast}
	b := &recoveryConn{closed: make(chan struct{}), reading: make(chan struct{}), released: slow}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Join(ctx, a, b) }()
	<-a.reading
	<-b.reading
	cancel()
	select {
	case <-done:
		close(slow)
		t.Fatal("Join returned while one copy was still running")
	case <-time.After(30 * time.Millisecond):
	}
	close(slow)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Join did not finish after both copies ended")
	}
}
