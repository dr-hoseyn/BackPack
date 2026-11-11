package network

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestWatchedMuxKeepsBufferedBytesAfterBothDirectionsFinish(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, copyOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("v%d/copy=%t", version, copyOut), func(t *testing.T) {
				left, right := net.Pipe()
				cfg := smux.DefaultConfig()
				cfg.Version = version
				cfg.KeepAliveDisabled = true
				client, err := NewWatchedMuxSession(left, cfg, smux.Client)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { client.Close() })
				server, err := NewWatchedMuxSession(right, cfg, smux.Server)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { server.Close() })
				client.SetDeadline(time.Now().Add(3 * time.Second))
				server.SetDeadline(time.Now().Add(3 * time.Second))
				sent, err := client.OpenStream()
				if err != nil {
					t.Fatal(err)
				}
				received, err := server.AcceptStream()
				if err != nil {
					t.Fatal(err)
				}
				received.SetReadDeadline(time.Now().Add(3 * time.Second))
				payload := bytes.Repeat([]byte("buffered-tail"), 2048)
				if n, err := sent.Write(payload); n != len(payload) || err != nil {
					t.Fatalf("write %d: %v", n, err)
				}
				if err := sent.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				if err := received.CloseWrite(); err != nil {
					t.Fatal(err)
				}
				// This SYN follows the first stream's FIN on the same wire.
				// Accepting it is an in-band barrier: no sleeps or private state
				// are needed to know the server processed the earlier FIN.
				barrier, err := client.OpenStream()
				if err != nil {
					t.Fatal(err)
				}
				defer barrier.Close()
				peerBarrier, err := server.AcceptStream()
				if err != nil {
					t.Fatal(err)
				}
				defer peerBarrier.Close()
				var got bytes.Buffer
				if copyOut {
					_, err = io.Copy(&got, received)
				} else {
					var chunk [1024]byte
					for {
						n, readErr := received.Read(chunk[:])
						got.Write(chunk[:n])
						if readErr == io.EOF {
							break
						}
						if readErr != nil {
							err = readErr
							break
						}
					}
				}
				if err != nil || !bytes.Equal(got.Bytes(), payload) {
					t.Fatalf("both FINs lost buffered bytes: got %d of %d, error %v", got.Len(), len(payload), err)
				}
				if server.NumStreams() != 1 {
					t.Fatalf("drained stream retained in pool: %d streams", server.NumStreams())
				}
			})
		}
	}
}
