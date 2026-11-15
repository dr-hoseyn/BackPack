package bridge

import (
	"context"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

type observedReadConn struct {
	net.Conn
	started chan<- struct{}
	once    sync.Once
}

func (c *observedReadConn) Read(p []byte) (int, error) {
	c.once.Do(func() { c.started <- struct{}{} })
	return c.Conn.Read(p)
}

// A busy forwarder should retain at most two goroutines per idle flow,
// including the caller, while still running both directional reads.
func TestIdleJoinsStayWithinTheirWorkerBudget(t *testing.T) {
	const flows = 32
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2*flows)
	done := make(chan struct{}, flows)
	var ends []net.Conn
	t.Cleanup(func() {
		cancel()
		for _, conn := range ends {
			conn.Close()
		}
		for i := 0; i < flows; i++ {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("a cancelled join did not release its workers")
				return
			}
		}
	})
	baseline := runtime.NumGoroutine()
	for i := 0; i < flows; i++ {
		user, a := net.Pipe()
		b, backend := net.Pipe()
		ends = append(ends, user, a, b, backend)
		go func() {
			Join(ctx, &observedReadConn{Conn: a, started: started}, &observedReadConn{Conn: b, started: started})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 2*flows; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("both directions did not start for every join")
		}
	}
	delta := runtime.NumGoroutine() - baseline
	t.Logf("idle flows=%d, additional goroutines=%d", flows, delta)
	// Allow unrelated runtime/background activity without admitting a third
	// parked worker for every flow.
	if delta > 2*flows+8 {
		t.Fatalf("idle joins retained %d goroutines for %d flows", delta, flows)
	}
}

func TestJoinStreamEOFReleasesIdleOppositeDirection(t *testing.T) {
	for _, first := range []string{"a", "b"} {
		t.Run(first, func(t *testing.T) {
			user, a := net.Pipe()
			b, backend := net.Pipe()
			t.Cleanup(func() { user.Close(); a.Close(); b.Close(); backend.Close() })
			done := make(chan struct{})
			go func() { Join(context.Background(), a, b); close(done) }()
			if first == "a" {
				user.Close()
			} else {
				backend.Close()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("one-sided stream EOF left the opposite copy blocked")
			}
		})
	}
}
