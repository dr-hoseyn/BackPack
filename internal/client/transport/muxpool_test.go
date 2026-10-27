package transport

import (
	"net"
	"testing"

	"github.com/xtaci/smux"
)

func TestMuxPoolTracksRealStreamsAndLegacyRetirement(t *testing.T) {
	a, b := net.Pipe()
	session, err := smux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	peer, err := smux.Server(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var pool muxPoolSessions
	remove := pool.add(session, true)
	if active, total, supported := pool.snapshot(); active != 0 || total != 1 || !supported {
		t.Fatalf("idle snapshot = %d/%d/%v", active, total, supported)
	}
	stream, err := session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if active, total, _ := pool.snapshot(); active != 1 || total != 1 {
		t.Fatal("open stream was not counted as an occupied session")
	}
	stream.Close()
	remove()
	remove = pool.add(session, false)
	if _, _, supported := pool.snapshot(); supported {
		t.Fatal("legacy peer opted into retirement")
	}
	remove()
	if _, total, supported := pool.snapshot(); total != 0 || supported {
		t.Fatal("session outlived its handler")
	}
}
