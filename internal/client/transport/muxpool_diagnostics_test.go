package transport

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xtaci/smux"
)

func diagnosticMuxPair(t *testing.T) (*smux.Session, *smux.Session) {
	t.Helper()
	a, b := net.Pipe()
	local, err := smux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := smux.Server(b, nil)
	if err != nil {
		local.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close(); peer.Close() })
	return local, peer
}

func TestWSMuxPoolDiagnosticsPreserveUnknownAndSilentStreams(t *testing.T) {
	local, _ := diagnosticMuxPair(t)
	empty, _ := diagnosticMuxPair(t)
	var observations atomic.Int32
	var pool muxPoolSessions
	removeLocal := pool.addObserved(local, true, func() (int, bool) { observations.Add(1); return 0, false })
	defer removeLocal()
	removeEmpty := pool.addObserved(empty, true, func() (int, bool) { observations.Add(1); return 3, true })
	defer removeEmpty()
	stream, err := local.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if s := pool.sample(false); s.total != 2 || s.active != 1 || s.streams != 1 || s.pressureObserved != 0 || observations.Load() != 0 {
		t.Fatalf("default sample changed occupancy or observed sockets: %+v", s)
	}
	if s := pool.sample(true); s.total != 2 || s.active != 1 || s.streams != 1 || s.pressureObserved != 1 || s.pressured != 1 {
		t.Fatalf("unknown socket or silent stream was misreported: %+v", s)
	}
	empty.Close()
	if s := pool.sample(true); s.pressureObserved != 0 || s.pressured != 0 {
		t.Fatalf("closed socket was reported as healthy: %+v", s)
	}
}

func TestWSMuxPoolDiagnosticsConcurrentRegistration(t *testing.T) {
	a, _ := diagnosticMuxPair(t)
	b, _ := diagnosticMuxPair(t)
	var pool muxPoolSessions
	remove := pool.add(a, true)
	defer remove()
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 1000 {
			remove := pool.addObserved(b, true, func() (int, bool) { return 0, true })
			remove()
		}
	})
	for range 1000 {
		s := pool.sample(true)
		if s.total < 1 || s.total > 2 || s.active != 0 || s.pressureObserved > s.total {
			t.Fatalf("inconsistent concurrent sample: %+v", s)
		}
	}
	workers.Wait()
}
