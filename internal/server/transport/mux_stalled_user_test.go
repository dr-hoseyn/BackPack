package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils"
	"github.com/xtaci/smux"
)

type muxStalledUser struct {
	net.Conn
	writing chan struct{}
	once    sync.Once
}

func (c *muxStalledUser) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.writing) })
	return c.Conn.Write(p)
}

func TestMuxSessionFailureReleasesStalledUser(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			owner, cancel := context.WithCancel(t.Context())
			defer cancel()
			a, b := net.Pipe()
			cfg := smux.DefaultConfig()
			cfg.Version = version
			session, err := smux.Client(a, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			peer, err := smux.Server(b, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			local, user := net.Pipe()
			defer user.Close()
			stalled := &muxStalledUser{Conn: local, writing: make(chan struct{})}
			defer stalled.Close()
			limits := newLimiter(Limits{MaxConnections: 1})
			if !limits.acquire() {
				t.Fatal("reserve connection")
			}
			queue := newBalancedMuxLocalQueue(1)
			queue.push(LocalTCPConn{conn: stalled, timeCreated: nowMillis(), remoteAddr: "127.0.0.1:80"}, nil)
			streams, sessions := int32(1), int32(1)
			loop := muxSession{ctx: owner, local: queue, muxCon: 1, limits: limits, cancelOnFailure: true,
				log: quietLogger(), streams: &streams, sessions: &sessions}
			done := make(chan struct{})
			go func() { defer close(done); loop.run(session) }()
			t.Cleanup(func() { cancel(); stalled.Close(); session.Close(); <-done })
			peer.SetDeadline(time.Now().Add(2 * time.Second))
			stream, err := peer.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := utils.ReceiveBinaryString(stream); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Write([]byte("response")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-stalled.writing:
			case <-time.After(2 * time.Second):
				t.Fatal("relay never reached user write")
			}
			// Keep the control owner alive and never read the user socket.
			// Closing smux alone cannot interrupt that socket's blocked Write.
			session.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("failed session retained a stalled user and its quota")
			}
			if owner.Err() != nil {
				t.Fatal("data failure cancelled the control owner")
			}
			if atomic.LoadInt32(&streams) != 0 || atomic.LoadInt32(&sessions) != 0 || limits.active.Load() != 0 {
				t.Fatal("failed session retained counters or connection quota")
			}
		})
	}
}

func TestWSSMuxSessionCancellationScope(t *testing.T) {
	for _, mode := range []config.TransportType{config.WSMUX, config.WSSMUX} {
		s := &WsMuxTransport{config: &WsMuxConfig{Mode: mode}, smuxConfig: smux.DefaultConfig()}
		g := &wsMuxGen{ctx: t.Context()}
		loop := s.session(g, g.ctx)
		if loop.cancelOnFailure != (mode == config.WSSMUX) {
			t.Fatalf("unexpected session cancellation policy for %s", mode)
		}
	}
}
