package transport

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A second Close may return immediately while the first is still unblocking
// socket I/O. Model that behavior without depending on scheduler timing.
type delayedListenerClose struct {
	calls   atomic.Int32
	started chan struct{}
	allow   chan struct{}
}

func (l *delayedListenerClose) Close() error {
	if l.calls.Add(1) == 1 {
		close(l.started)
		<-l.allow
	}
	return nil
}

func TestListenerCloseCleanupWaitsForCancellationClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := &delayedListenerClose{started: make(chan struct{}), allow: make(chan struct{})}
	defer close(listener.allow)
	cleanup := closeListenerOnCancel(ctx, listener)
	cancel()
	select {
	case <-listener.started:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not begin closing the listener")
	}
	done := make(chan struct{})
	go func() { cleanup(); close(done) }()
	select {
	case <-done:
		t.Fatal("ownership released while cancellation was still closing the listener")
	case <-time.After(30 * time.Millisecond):
	}
	listener.allow <- struct{}{}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after the descriptor was released")
	}
	cleanup()
	if calls := listener.calls.Load(); calls != 1 {
		t.Fatalf("listener closed %d times, want once", calls)
	}
}

func TestListenerCloseCleanupDetachesLiveContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := &delayedListenerClose{started: make(chan struct{}), allow: make(chan struct{})}
	close(listener.allow)
	cleanup := closeListenerOnCancel(ctx, listener)
	cleanup()
	cancel()
	cleanup()
	if calls := listener.calls.Load(); calls != 1 {
		t.Fatalf("listener closed %d times, want once", calls)
	}
}
