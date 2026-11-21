package transport

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/gorilla/websocket"
)

// Exercise the actual TLS listener, control seat and recovery callback. A
// peer that drains heartbeats but never echoes them must not stay connected
// merely because the server's writes continue to succeed.
func TestWSSMuxSilentControlVacatesSeatWithoutRestartingListener(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := listenerRecoveryServer(t, config.WSSMUX, freeAddr(t), ctx)
	generation := s.run.context()
	g := s.newGen(generation)
	s.start(g)
	t.Cleanup(func() {
		cancel()
		s.run.stop()
		s.Wait(context.Background())
	})

	dial := func() *websocket.Conn {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := network.WebSocketDialer(ctx, nil, s.config.BindAddr, "", "/channel",
				time.Second, time.Second, true, s.config.Token, config.WSSMUX, false, 1, 0, 0, 0)
			if err == nil {
				return conn
			}
			if time.Now().After(deadline) {
				t.Fatalf("control handshake failed: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	conn := dial()
	defer conn.Close()
	// The HTTP upgrade response can arrive before the handler installs the seat.
	deadline := time.Now().Add(time.Second)
	for !s.Running() {
		if time.Now().After(deadline) {
			t.Fatal("authenticated control channel never acquired the seat")
		}
		time.Sleep(time.Millisecond)
	}
	owner := g.seat.client()
	var received atomic.Int32
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			received.Add(1)
		}
	}()
	select {
	case <-owner.Done():
	case <-time.After(35 * time.Second):
		t.Fatal("silent TLS control peer retained its seat")
	}
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("vacating the seat did not close its control socket")
	}
	if received.Load() < 2 {
		t.Fatal("fixture did not drain successful outbound heartbeats")
	}
	if generation.Err() != nil || s.run.context() != generation {
		t.Fatal("control silence restarted the tunnel listener")
	}

	replacement := dial()
	defer replacement.Close()
	_ = replacement.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := replacement.ReadMessage(); err != nil {
		t.Fatalf("listener did not serve the replacement control peer: %v", err)
	}
}

func TestWSSMuxAcknowledgesDataAdmissionAndReleasesCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := listenerRecoveryServer(t, config.WSSMUX, freeAddr(t), ctx)
	s.config.MaxSessions = 1
	g := s.newGen(s.run.context())
	s.start(g)
	t.Cleanup(func() {
		cancel()
		s.run.stop()
		s.Wait(context.Background())
	})
	dial := func(path string, opts ...network.WSDialOption) (*websocket.Conn, error) {
		return network.WebSocketDialer(ctx, nil, s.config.BindAddr, "", path,
			time.Second, time.Second, true, s.config.Token, config.WSSMUX, false, 1, 0, 0, 0, opts...)
	}
	wait := func(ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ready() {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for WSSMux state")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var control *websocket.Conn
	wait(func() bool {
		control, _ = dial("/channel")
		return control != nil
	})
	defer control.Close()
	wait(s.Running)
	dialData := func() (*websocket.Conn, error) {
		return dial("/tunnel", &network.WSMuxOptions{PoolSize: 1, ConfirmAdmission: true})
	}
	first, err := dialData()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := dialData(); err == nil {
		second.Close()
		t.Fatal("data carrier exceeded the physical admission limit")
	}
	first.Close()
	wait(func() bool { return len(g.admissionSlots) == 0 })
	replacement, err := dialData()
	if err != nil {
		t.Fatalf("released capacity rejected a new carrier: %v", err)
	}
	replacement.Close()
}
