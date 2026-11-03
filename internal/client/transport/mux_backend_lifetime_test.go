package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils"
	"github.com/xtaci/smux"
)

func TestMuxSessionFailureCancelsBackendDNSWithoutEndingSiblingSessions(t *testing.T) {
	for _, target := range []string{"mux-session-backend.invalid:443", "udp://mux-session-backend.invalid:53"} {
		t.Run(target, func(t *testing.T) {
			original := net.DefaultResolver
			started := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
					return nil, errors.New("test resolver released")
				}
			}}
			ctx, cancel := context.WithCancel(t.Context())
			var l lifecycle
			l.firstGeneration(ctx, silentLogger(), usageSpec{})
			t.Cleanup(func() {
				cancel()
				releaseOnce.Do(func() { close(release) })
				l.state.Stop()
				l.state.Wait()
				net.DefaultResolver = original
			})
			newSession := func() (*smux.Session, *smux.Session) {
				a, b := net.Pipe()
				server, err := smux.Client(a, nil)
				if err != nil {
					t.Fatal(err)
				}
				client, err := smux.Server(b, nil)
				if err != nil {
					server.Close()
					t.Fatal(err)
				}
				t.Cleanup(func() { server.Close(); client.Close() })
				l.state.Go(func() {
					l.serveSession(client, b.RemoteAddr(), backendOpts{dialTimeout: 30 * time.Second, keepAlive: time.Second})
				})
				return server, client
			}
			failed, failedClient := newSession()
			sibling, _ := newSession()
			stream, err := failed.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if err := utils.SendBinaryString(stream, target); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("backend DNS did not start")
			}
			failed.Close()
			deadline := time.Now().Add(time.Second)
			for !failedClient.IsClosed() {
				if time.Now().After(deadline) {
					t.Fatal("failed mux session was not retired")
				}
				time.Sleep(time.Millisecond)
			}
			// Keep the second physical session alive: a whole-generation stop
			// would release DNS too, but would also disrupt unrelated users.
			user, err := sibling.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			if err := utils.SendBinaryString(user, echoService(t)); err != nil {
				t.Fatal(err)
			}
			_ = user.SetDeadline(time.Now().Add(time.Second))
			if _, err := user.Write([]byte("sibling")); err != nil {
				t.Fatal(err)
			}
			var reply [7]byte
			if _, err := io.ReadFull(user, reply[:]); err != nil || string(reply[:]) != "sibling" {
				t.Fatalf("sibling physical session stopped carrying traffic: %q %v", reply[:], err)
			}
			user.Close()
			sibling.Close()
			// Once both session owners have stopped, all their backend work
			// must join without cancelling the still-live generation or waiting
			// for its 30-second backend dial timeout.
			joined := make(chan struct{})
			go func() { l.state.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(time.Second):
				t.Fatal("a retired data session retained backend DNS until the generation ended")
			}
			if l.state.Ctx().Err() != nil {
				t.Fatal("retiring data sessions cancelled the control generation")
			}
		})
	}
}
