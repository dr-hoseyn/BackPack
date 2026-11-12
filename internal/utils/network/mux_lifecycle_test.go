package network

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The peer may answer a SYN before the local socket's Write returns. Keep
// that write pending while the independent receive loop processes the reply.
type delayedFirstMuxWrite struct {
	net.Conn
	release <-chan struct{}
	first   bool // accessed only by smux's single sender
}

func (c *delayedFirstMuxWrite) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if !c.first {
		c.first = true
		<-c.release
	}
	return n, err
}

func TestWatchedMuxKeepsAPeerReplyBeforeOpenReturns(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			left, right := net.Pipe()
			allowed := make(chan struct{})
			release := sync.OnceFunc(func() { close(allowed) })
			defer release() // also unblock the sender before cleanup on failure
			cfg := smux.DefaultConfig()
			cfg.Version, cfg.KeepAliveDisabled = version, true
			client, err := NewWatchedMuxSession(&delayedFirstMuxWrite{Conn: left, release: allowed}, cfg, smux.Client)
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
			type openedStream struct {
				stream *smux.Stream
				err    error
			}
			opened := make(chan openedStream, 1)
			go func() {
				stream, err := client.OpenStream()
				opened <- openedStream{stream, err}
			}()
			peer, err := server.AcceptStream()
			if err != nil {
				t.Fatal(err)
			}
			greeting := []byte("immediate peer reply")
			if _, err := peer.Write(greeting); err != nil {
				t.Fatal(err)
			}
			// A subsequent peer SYN is an in-band barrier proving the client's
			// receiver processed the greeting while its OpenStream was pending.
			if _, err := server.OpenStream(); err != nil {
				t.Fatal(err)
			}
			if _, err := client.AcceptStream(); err != nil {
				t.Fatal(err)
			}
			release()
			result := <-opened
			if result.err != nil {
				t.Fatal(result.err)
			}
			result.stream.SetReadDeadline(time.Now().Add(time.Second))
			got := make([]byte, len(greeting))
			if _, err := io.ReadFull(result.stream, got); err != nil || !bytes.Equal(got, greeting) {
				t.Fatalf("early peer reply lost: got %q, error %v", got, err)
			}
		})
	}
}

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
