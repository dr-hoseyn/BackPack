package handlers

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/gorilla/websocket"
)

// A fragmented message can remain open while delivering a continuous stream.
// Pool adaptation needs those bytes during the transfer, not at message EOF.
func TestWebSocketReportsProgressBeforeAFragmentedMessageEnds(t *testing.T) {
	client, server := websocketPair(t)
	relay, backend := net.Pipe()
	t.Cleanup(func() { relay.Close(); backend.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		transferWebSocketToTCP(server, relay, quietLogger(), nil, 0, false)
	}()
	t.Cleanup(func() {
		client.Close()
		backend.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("fragmented-message relay did not stop")
		}
	})
	_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = backend.SetDeadline(time.Now().Add(5 * time.Second))
	inBefore, outBefore := metrics.Traffic()
	w, err := client.NextWriter(websocket.BinaryMessage)
	if err != nil {
		t.Fatal(err)
	}
	// The extra byte forces Gorilla to flush the preceding complete buffers
	// as nonfinal fragments, while keeping the message open at the checkpoint.
	prefix := bytes.Repeat([]byte{0x39}, 3*relayBufferSize+1)
	written := make(chan error, 1)
	go func() { _, err := w.Write(prefix); written <- err }()
	got := make([]byte, len(prefix)-1)
	if _, err := io.ReadFull(backend, got); err != nil || !bytes.Equal(got, prefix[:len(got)]) {
		t.Fatalf("fragment prefix changed: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		in, out := metrics.Traffic()
		if in-inBefore == uint64(len(got)) && out == outBefore {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unfinished message delivered %d bytes but counters moved %d/%d", len(got), in-inBefore, out-outBefore)
		}
		time.Sleep(time.Millisecond)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var tail [1]byte
	if _, err := io.ReadFull(backend, tail[:]); err != nil || tail[0] != prefix[len(prefix)-1] {
		t.Fatalf("message tail changed: %x, %v", tail, err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("closed websocket retained its relay")
	}
	in, out := metrics.Traffic()
	if in-inBefore != uint64(len(prefix)) || out != outBefore {
		t.Fatalf("message end counted bytes twice: %d/%d", in-inBefore, out-outBefore)
	}
}

type partialWSBackend struct {
	net.Conn
	accepted []byte
	n        int
	err      error
}

func (c *partialWSBackend) Write(p []byte) (int, error) {
	n := min(c.n, len(p))
	c.accepted = append(c.accepted, p[:n]...)
	return n, c.err
}

func (*partialWSBackend) Close() error { return nil }

func TestWebSocketReportsOnlyBytesAcceptedByAFailingBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		err  error
	}{
		{"partial with error", 5, io.ErrClosedPipe},
		{"short write", 5, nil},
		{"nothing accepted", 0, io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := websocketPair(t)
			backend := &partialWSBackend{n: tc.n, err: tc.err}
			done := make(chan struct{})
			inBefore, outBefore := metrics.Traffic()
			go func() {
				defer close(done)
				transferWebSocketToTCP(server, backend, quietLogger(), nil, 0, false)
			}()
			payload := []byte("accepted prefix, rejected remainder")
			if err := client.WriteMessage(websocket.BinaryMessage, payload); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("failed backend retained websocket relay")
			}
			if !bytes.Equal(backend.accepted, payload[:tc.n]) {
				t.Fatal("failed write changed or replayed accepted bytes")
			}
			in, out := metrics.Traffic()
			if in-inBefore != uint64(tc.n) || out != outBefore {
				t.Fatalf("accepted %d bytes but counted %d/%d", tc.n, in-inBefore, out-outBefore)
			}
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			_, _, err := client.NextReader()
			if err == nil {
				t.Fatalf("failed backend did not end the remote stream: %v", err)
			}
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("failed backend left the remote stream waiting until its deadline")
			}
		})
	}
}
