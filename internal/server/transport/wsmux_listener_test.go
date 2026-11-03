package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
)

type failedMuxListener struct {
	net.Listener
	failed atomic.Bool
}

var errMuxAcceptFailure = errors.New("injected fatal mux listener failure")

func (l *failedMuxListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if l.failed.Load() && err != nil {
		return nil, errMuxAcceptFailure
	}
	return c, err
}

func listenerRecoveryServer(t *testing.T, mode config.TransportType, addr string, parent context.Context) *WsMuxTransport {
	t.Helper()
	return NewWSMuxServer(parent, &WsMuxConfig{
		BindAddr: addr, Token: "listener-recovery-token-0123456789", Mode: mode,
		ChannelSize: 8, MuxCon: 2, MuxVersion: 2, Nodelay: true,
		Heartbeat: time.Second, KeepAlive: time.Second,
		MaxFrameSize: 4096, MaxReceiveBuffer: 1 << 16, MaxStreamBuffer: 4096,
	}, quietLogger())
}

func TestWSMuxFatalListenerFailureReopensAnAuthenticatedTunnelPort(t *testing.T) {
	for _, mode := range []config.TransportType{config.WSMUX, config.WSSMUX} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			s := listenerRecoveryServer(t, mode, ln.Addr().String(), ctx)
			old := s.run.context()
			g := s.newGen(old)
			listener := &failedMuxListener{Listener: ln}
			httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })}
			if mode == config.WSSMUX {
				httpServer.TLSConfig, err = network.ServerTLSConfig(s.tlsSettings(), s.logger.Warnf)
				if err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan struct{})
			// Match tunnelListener's ownership: Restart must wait until the
			// failed HTTP loop has closed its sockets and released this slot.
			s.listeners.hold()
			go func() {
				defer close(done)
				defer s.listeners.release()
				s.serveTunnelListener(g, httpServer, listener)
			}()
			t.Cleanup(func() {
				cancel()
				httpServer.Close()
				s.run.stop()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("failed listener retained its supervisor")
				}
				s.Wait(context.Background())
			})
			// A permanent Accept error ends net/http's own temporary-error
			// retry. The tunnel must create a fresh generation automatically.
			listener.failed.Store(true)
			listener.Close()
			select {
			case <-old.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("fatal listener error left the generation apparently live")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				conn, err := network.WebSocketDialer(ctx, nil, s.config.BindAddr, "", "/channel", time.Second, time.Second, true,
					s.config.Token, mode, false, 1, 0, 0, 0)
				if err == nil {
					conn.Close()
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("replacement listener never authenticated a new control channel: %v", err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if s.run.context() == old {
				t.Fatal("failure did not replace the old generation")
			}
		})
	}
}

func TestWSMuxDelayedListenerRecoveryCannotRestartANewerGeneration(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := listenerRecoveryServer(t, config.WSMUX, freeAddr(t), ctx)
	old := s.run.context()
	s.Restart()
	current := s.run.context()
	t.Cleanup(func() { cancel(); s.run.stop(); s.Wait(context.Background()) })
	s.restartGeneration(old)
	if s.run.context() != current || current.Err() != nil {
		t.Fatal("a delayed failure from the old listener ended its replacement")
	}
}

func TestWSMuxListenerShutdownDoesNotStartAReplacementGeneration(t *testing.T) {
	for _, mode := range []config.TransportType{config.WSMUX, config.WSSMUX} {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			s := listenerRecoveryServer(t, mode, ln.Addr().String(), ctx)
			old := s.run.context()
			httpServer := &http.Server{}
			if mode == config.WSSMUX {
				httpServer.TLSConfig, err = network.ServerTLSConfig(s.tlsSettings(), s.logger.Warnf)
				if err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.serveTunnelListener(s.newGen(old), httpServer, ln)
			}()
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("intentional shutdown left the listener supervisor blocked")
			}
			if s.run.context() != old {
				t.Fatal("intentional shutdown created a replacement generation")
			}
		})
	}
}
