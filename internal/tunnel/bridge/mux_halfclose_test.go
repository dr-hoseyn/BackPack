package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/tunnel/limits"
	"github.com/xtaci/smux"
)

func TestJoinPreservesMuxHalfCloseThroughWrappers(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, capped := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/capped-%t", version, capped), func(t *testing.T) {
				client, server := tcpPair(t)
				cfg := smux.DefaultConfig()
				cfg.Version = version
				sender, err := smux.Client(client, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer sender.Close()
				receiver, err := smux.Server(server, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer receiver.Close()
				stream, err := sender.OpenStream()
				if err != nil {
					t.Fatal(err)
				}
				peer, err := receiver.AcceptStream()
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
				user, local := tcpPair(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var limiter *limits.Limiter
				if capped {
					limiter = limits.New(limits.Config{BandwidthMbps: 1})
				}
				done := make(chan struct{})
				go func() {
					Join(ctx, limiter.Wrap(ctx, local), limiter.Wrap(ctx, metrics.CountedConn(stream)))
					close(done)
				}()
				user.SetDeadline(time.Now().Add(3 * time.Second))
				peer.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := user.Write([]byte("request")); err != nil {
					t.Fatal(err)
				}
				if err := user.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				request, err := io.ReadAll(peer)
				if err != nil || string(request) != "request" {
					t.Fatalf("upload EOF: request=%q err=%v", request, err)
				}
				response := bytes.Repeat([]byte("reply"), 8192)
				if _, err := peer.Write(response); err != nil {
					t.Fatal(err)
				}
				if err := peer.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(user)
				if err != nil || !bytes.Equal(got, response) {
					t.Fatalf("response after EOF: bytes=%d want=%d err=%v", len(got), len(response), err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("completed half-close retained relay workers")
				}
			})
		}
	}
}
