package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
)

// Join the two reverse relays around a real mux session. The backend waits for
// upload EOF before responding, so neither relay may fully close its stream.
func TestMuxRelaysReturnResponseAfterUploadEOF(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, sniffer := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/sniffer-%t", version, sniffer), func(t *testing.T) {
				send, receive := muxRelaySource(t, version)
				entry, user := muxTCPDestination(t)
				backendEnd, backend := muxTCPDestination(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var workers sync.WaitGroup
				workers.Go(func() {
					TCPConnectionHandler(ctx, false, entry, metrics.CountedConn(send), quietLogger(), nil, 0, sniffer)
				})
				workers.Go(func() {
					TCPConnectionHandler(ctx, false, metrics.CountedConn(receive), backendEnd, quietLogger(), nil, 0, sniffer)
				})
				done := make(chan struct{})
				go func() { workers.Wait(); close(done) }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Error("mux relay workers did not stop")
					}
				})
				user.SetDeadline(time.Now().Add(3 * time.Second))
				backend.SetDeadline(time.Now().Add(3 * time.Second))
				upload := bytes.Repeat([]byte("upload"), 8192)
				if _, err := user.Write(upload); err != nil {
					t.Fatal(err)
				}
				if err := user.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(backend)
				if err != nil || !bytes.Equal(got, upload) {
					t.Fatalf("upload EOF: bytes=%d want=%d err=%v", len(got), len(upload), err)
				}
				response := bytes.Repeat([]byte("response after EOF"), 8192)
				if _, err := backend.Write(response); err != nil {
					t.Fatal(err)
				}
				if err := backend.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				got, err = io.ReadAll(user)
				if err != nil || !bytes.Equal(got, response) {
					t.Fatalf("response after EOF: bytes=%d want=%d err=%v", len(got), len(response), err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("completed mux exchange retained a relay")
				}
			})
		}
	}
}
