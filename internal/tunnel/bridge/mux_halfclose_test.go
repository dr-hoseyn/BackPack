package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/xtaci/smux"
)

func muxStreamPair(t *testing.T, version int) (*smux.Stream, *smux.Stream) {
	t.Helper()
	client, server := tcpPair(t)
	cfg := smux.DefaultConfig()
	cfg.Version = version
	sender, err := smux.Client(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sender.Close() })
	receiver, err := smux.Server(server, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { receiver.Close() })
	stream, err := sender.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := receiver.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close(); peer.Close() })
	return stream, peer
}

func TestJoinPreservesMuxHalfCloseThroughWrappers(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			stream, peer := muxStreamPair(t, version)
			user, local := tcpPair(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				Join(ctx, local, metrics.CountedConn(stream))
				close(done)
			}()
			user.SetDeadline(time.Now().Add(3 * time.Second))
			peer.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := user.Write([]byte("request")); err != nil {
				t.Fatal(err)
			}
			if err := user.(*net.TCPConn).CloseWrite(); err != nil {
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

func TestJoinMuxHalfCloseEndsWithContext(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			stream, peer := muxStreamPair(t, version)
			user, local := tcpPair(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { Join(ctx, local, metrics.CountedConn(stream)); close(done) }()
			peer.SetReadDeadline(time.Now().Add(time.Second))
			if err := user.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(peer); err != nil {
				t.Fatalf("upload EOF did not reach mux peer: %v", err)
			}
			// The peer leaves its response direction open. Cancelling must
			// release the remaining read rather than wait for a second FIN.
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("half-closed mux relay outlived cancellation")
			}
		})
	}
}
