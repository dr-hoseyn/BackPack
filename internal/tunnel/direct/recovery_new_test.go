package direct

import (
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/xtaci/smux"
)

func TestRecoveryNewBlockedSocketWriteExpires(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	watched := newWatchedConn(a, 25*time.Millisecond)
	done := make(chan error, 1)
	go func() { _, err := watched.Write([]byte("blocked")); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("underlying socket write never expired")
	}
	select {
	case <-watched.dead:
	default:
		t.Fatal("the write failure did not kill the session")
	}
}

func TestRecoveryNewPartialSessionLossKeepsPeer(t *testing.T) {
	var sessions sessionSet
	makeSession := func() *tunnelSession {
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		go func() {
			buf := make([]byte, 4096)
			for {
				if _, err := b.Read(buf); err != nil {
					return
				}
			}
		}()
		cfg := smux.DefaultConfig()
		cfg.KeepAliveDisabled = true
		watched := newWatchedConn(a, time.Second)
		s, err := smux.Client(watched, cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return &tunnelSession{Session: s, dead: watched.dead}
	}
	a, b := makeSession(), makeSession()
	sessions.add(a)
	sessions.add(b)
	a.Close()
	sessions.remove(a)
	if got := metrics.SnapshotConnected(); got == nil || !*got {
		t.Fatal("losing one session reported the whole tunnel disconnected")
	}
	b.Close()
	sessions.remove(b)
	if got := metrics.SnapshotConnected(); got == nil || *got {
		t.Fatal("losing the last session was not reported")
	}
}
