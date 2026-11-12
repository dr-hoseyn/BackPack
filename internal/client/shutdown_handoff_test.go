package client

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/server"
	"github.com/sirupsen/logrus"
)

// Hold a real relay worker at the log it emits after the forwarded user closes.
// The owner must join that worker, rather than treating cancellation itself as
// proof of completion. The hook changes no socket, payload or transport state.
type pausedRelayCleanup struct {
	once    sync.Once
	entered chan struct{}
	release <-chan struct{}
}

func (*pausedRelayCleanup) Levels() []logrus.Level { return logrus.AllLevels }
func (h *pausedRelayCleanup) Fire(entry *logrus.Entry) error {
	if entry.Message == "reader stream closed or EOF received" {
		h.once.Do(func() { close(h.entered); <-h.release })
	}
	return nil
}

func TestClientStartWaitsForItsRealMuxRelayToFinish(t *testing.T) {
	for _, mode := range []config.TransportType{config.WSMUX, config.WSSMUX} {
		t.Run(string(mode), func(t *testing.T) {
			backend, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			go func() {
				for {
					c, err := backend.Accept()
					if err != nil {
						return
					}
					go func() { defer c.Close(); io.Copy(c, c) }()
				}
			}()
			freeAddress := func() string {
				ln, err := net.Listen("tcp4", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := ln.Addr().String()
				ln.Close()
				return addr
			}
			remote, entry := freeAddress(), freeAddress()
			_, entryPort, _ := net.SplitHostPort(entry)
			const token = "shutdown-handoff-test-token"
			srv := server.NewServer(&config.ServerConfig{
				BindAddr: remote, Transport: mode, Token: token,
				Ports:       []string{fmt.Sprintf("%s=%s", entryPort, backend.Addr())},
				ChannelSize: 32, Heartbeat: 2, Keepalive: 8, MuxCon: 8, MuxSession: 8,
				MuxVersion: 2, MaxFrameSize: 32768, MaxReceiveBuffer: 4 << 20, MaxStreamBuffer: 65536,
				Nodelay: true, LogLevel: "fatal", SkipOptz: true,
			}, t.Context())
			serverDone := make(chan struct{})
			go func() { defer close(serverDone); srv.Start() }()
			cfg := baseConfig(mode)
			cfg.RemoteAddr, cfg.Token, cfg.LogLevel = remote, token, "trace"
			cfg.ConnectionPool, cfg.WSMuxMaxSessions = 1, 1
			cfg.WSSHTTP11Only, cfg.SkipOptz = true, true
			cli := NewClient(cfg, t.Context())
			cli.logger.SetOutput(io.Discard)
			release := make(chan struct{})
			resume := sync.OnceFunc(func() { close(release) })
			defer resume()
			hook := &pausedRelayCleanup{entered: make(chan struct{}), release: release}
			cli.logger.AddHook(hook)
			clientDone := make(chan struct{})
			go func() { defer close(clientDone); cli.Start() }()
			t.Cleanup(func() {
				resume()
				cli.Stop()
				srv.Stop()
				for _, done := range []<-chan struct{}{clientDone, serverDone} {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("engine did not stop")
					}
				}
			})
			deadline := time.Now().Add(10 * time.Second)
			var user net.Conn
			for time.Now().Before(deadline) {
				c, err := net.DialTimeout("tcp4", entry, 200*time.Millisecond)
				if err == nil {
					c.SetDeadline(time.Now().Add(500 * time.Millisecond))
					_, err = io.WriteString(c, "ready")
					var reply [5]byte
					if err == nil {
						_, err = io.ReadFull(c, reply[:])
					}
					if err == nil && string(reply[:]) == "ready" {
						user = c
						break
					}
					c.Close()
				}
				time.Sleep(10 * time.Millisecond)
			}
			if user == nil {
				t.Fatal("real mux tunnel never carried the readiness payload")
			}
			defer user.Close()
			user.Close()
			select {
			case <-hook.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("no relay cleanup was observed")
			}
			cli.Stop()
			select {
			case <-clientDone:
				t.Fatal("Client.Start returned while a real relay worker was still held")
			case <-time.After(50 * time.Millisecond):
			}
			resume()
			select {
			case <-clientDone:
			case <-time.After(3 * time.Second):
				t.Fatal("Client.Start did not return after its relay finished")
			}
		})
	}
}
