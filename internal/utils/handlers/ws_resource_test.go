package handlers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketRelayCarriesBothDirectionsAndJoinsOnPeerClose(t *testing.T) {
	client, server := websocketPair(t)
	relay, backend := tcpPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		WSConnectionHandler(ctx, server, relay, quietLogger(), nil, 0, false)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("websocket relay retained a copy worker")
		}
	}()
	request := bytes.Repeat([]byte{0x41}, 3*relayBufferSize)
	response := bytes.Repeat([]byte{0x92}, 3*relayBufferSize)
	_ = client.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	_ = backend.SetDeadline(time.Now().Add(3 * time.Second))
	writes := make(chan error, 2)
	go func() { writes <- client.WriteMessage(websocket.BinaryMessage, request) }()
	go func() { _, err := backend.Write(response); writes <- err }()
	gotRequest := make([]byte, len(request))
	if _, err := io.ReadFull(backend, gotRequest); err != nil {
		t.Fatal(err)
	}
	var gotResponse []byte
	for len(gotResponse) < len(response) {
		kind, p, err := client.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage {
			t.Fatalf("websocket response: kind=%d error=%v", kind, err)
		}
		gotResponse = append(gotResponse, p...)
	}
	for range 2 {
		if err := <-writes; err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(request, gotRequest) || !bytes.Equal(response, gotResponse) {
		t.Fatal("full-duplex websocket relay changed the payload")
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket peer close did not join both copies")
	}
	if _, err := backend.Read(make([]byte, 1)); err == nil {
		t.Fatal("backend remained open after websocket relay returned")
	}
}

func TestWebSocketRelayGrowsOnlyAfterAFullRead(t *testing.T) {
	for _, size := range []int{0, 17, smallRelayBufferSize, 3 * relayBufferSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			client, server := websocketPair(t)
			payload := bytes.Repeat([]byte{0x91}, size)
			from := &sizedReadConn{data: payload}
			done := make(chan struct{})
			go func() {
				defer close(done)
				transferTCPToWebSocket(from, server, quietLogger(), nil, 0, false)
			}()
			_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
			var got []byte
			for len(got) < size {
				kind, chunk, err := client.ReadMessage()
				if err != nil || kind != websocket.BinaryMessage {
					t.Fatalf("websocket payload: kind=%d error=%v", kind, err)
				}
				got = append(got, chunk...)
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("websocket sender did not stop at EOF")
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("adaptive websocket copy corrupted the stream")
			}
			if from.sizes[0] != smallRelayBufferSize {
				t.Fatalf("quiet websocket direction retained %d bytes", from.sizes[0])
			}
			if size < smallRelayBufferSize && slices.Contains(from.sizes, relayBufferSize) {
				t.Fatal("short websocket response acquired a bulk buffer")
			}
			if size >= smallRelayBufferSize && from.sizes[1] != relayBufferSize {
				t.Fatal("full websocket read did not grow for bulk traffic")
			}
		})
	}
}

func TestWebSocketReceiveReusesBuffersAcrossMessages(t *testing.T) {
	client, server := websocketPair(t)
	relay, backend := net.Pipe()
	defer backend.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		transferWebSocketToTCP(server, relay, quietLogger(), nil, 0, false)
	}()
	for i, size := range []int{0, 17, 3 * relayBufferSize, 29} {
		payload := bytes.Repeat([]byte{byte(i + 1)}, size)
		written := make(chan error, 1)
		go func() { written <- client.WriteMessage(websocket.BinaryMessage, payload) }()
		_ = backend.SetReadDeadline(time.Now().Add(3 * time.Second))
		got := make([]byte, size)
		if _, err := io.ReadFull(backend, got); err != nil {
			t.Fatal(err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("message %d changed while reusing a copy buffer", i)
		}
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("websocket receiver did not stop after peer close")
	}
}

type repeatedWebSocketSource struct {
	net.Conn
	remaining int
	size      int
}

func (c *repeatedWebSocketSource) Read(p []byte) (int, error) {
	if c.remaining == 0 {
		return 0, io.EOF
	}
	c.remaining--
	return min(len(p), c.size), nil
}
func (*repeatedWebSocketSource) Close() error { return nil }

// Real server-side websocket writes to a drained socket. Keep source reads
// above the boxed-int cache so disabled trace logging still has a cost if it
// is accidentally reintroduced on each iteration.
func BenchmarkWebSocketRelaySend(b *testing.B) {
	for _, size := range []int{1024, relayBufferSize} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			accepted := make(chan *websocket.Conn, 1)
			upgrader := websocket.Upgrader{WriteBufferSize: relayBufferSize}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				accepted <- ws
			}))
			defer srv.Close()
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				b.Fatal(err)
			}
			defer client.Close()
			server := <-accepted
			defer server.Close()
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for {
					_, r, err := client.NextReader()
					if err != nil {
						return
					}
					if _, err := io.Copy(io.Discard, r); err != nil {
						return
					}
				}
			}()
			from := &repeatedWebSocketSource{remaining: b.N, size: size}
			logger := quietLogger()
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			transferTCPToWebSocket(from, server, logger, nil, 0, false)
			b.StopTimer()
			client.Close()
			<-drained
		})
	}
}
