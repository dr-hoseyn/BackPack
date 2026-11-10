package transport

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestTCPMuxSessionLossWakesFloorRepair(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "data carrier lost"
		if canceled {
			name = "generation stopped"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := smux.DefaultConfig()
			client := NewMuxClient(ctx, &TcpMuxConfig{
				TCPFWD: true, MaxFrameSize: cfg.MaxFrameSize,
				MaxReceiveBuffer: cfg.MaxReceiveBuffer, MaxStreamBuffer: cfg.MaxStreamBuffer,
			}, silentLogger())
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			atomic.StoreInt32(&client.poolConnections, 1)
			done := make(chan struct{})
			go func() {
				client.handleSession(a, true)
				close(done)
			}()
			if canceled {
				cancel()
			} else {
				// EOF on one carrier leaves the control generation alive.
				b.Close()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("session handler did not return")
			}
			select {
			case <-client.poolLost:
				if canceled {
					t.Fatal("stopping a generation requested a replacement")
				}
			default:
				if !canceled {
					t.Fatal("lost carrier did not wake pool repair")
				}
			}
			if atomic.LoadInt32(&client.poolConnections) != 0 {
				t.Fatal("repair was notified before returning the pool slot")
			}
			if _, tracked, _ := client.sessions.snapshot(); tracked != 0 {
				t.Fatal("closed carrier remained in the pool sample")
			}
		})
	}
}

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
