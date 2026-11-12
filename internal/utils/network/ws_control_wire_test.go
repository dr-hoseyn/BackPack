package network

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func controlServerStream(tb testing.TB, raw net.Conn) *webSocketStream {
	tb.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://localhost/tunnel", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response := wsControlUpgrade{conn: raw, header: make(http.Header)}
	up := websocket.Upgrader{WriteBufferSize: wsWriteBufferSize, WriteBufferPool: wsWriteBuffers}
	conn, err := up.Upgrade(WSMuxResponse(response), request, nil)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { conn.Close() })
	return WebSocketStream(conn).(*webSocketStream)
}

type wsControlError struct {
	wsControlSink
	writeErr error
	writes   int
	closed   bool
}

func (c *wsControlError) Write(p []byte) (int, error) {
	c.writes++
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(p), nil
}
func (c *wsControlError) Close() error { c.closed = true; return nil }

func TestWSMuxHeaderOnlyFailureClosesItsCarrier(t *testing.T) {
	raw := &wsControlError{}
	stream := controlServerStream(t, raw)
	raw.writeErr = io.ErrClosedPipe
	parts := [][]byte{{2, 0, 0, 0, 1, 0, 0, 0}, nil}
	if n, err := stream.WriteBuffers(parts); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failed control frame: %d, %v", n, err)
	}
	if !raw.closed {
		t.Fatal("failed control frame left its carrier open")
	}
	writes := raw.writes
	if _, err := stream.WriteBuffers(parts); !errors.Is(err, io.ErrClosedPipe) || raw.writes != writes {
		t.Fatalf("failed framing resumed socket writes: %v", err)
	}
}

func TestWSMuxWindowUpdateFailureClosesItsCarrier(t *testing.T) {
	raw := &wsControlError{}
	stream := controlServerStream(t, raw)
	raw.writeErr = io.ErrClosedPipe
	parts := [][]byte{{2, 4, 8, 0, 1, 0, 0, 0}, {1, 0, 0, 0, 0, 64, 0, 0}}
	if n, err := stream.WriteBuffers(parts); n != 0 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failed window update: %d, %v", n, err)
	}
	if !raw.closed {
		t.Fatal("failed window update left its carrier open")
	}
}

func TestWSMuxHeaderOnlyAndPayloadFramesKeepTheirBytes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, side := range []string{"client", "server"} {
			name := "WS/" + side
			if secure {
				name = "WSS/" + side
			}
			t.Run(name, func(t *testing.T) {
				accepted := make(chan *websocket.Conn, 1)
				up := websocket.Upgrader{WriteBufferSize: wsWriteBufferSize, WriteBufferPool: wsWriteBuffers}
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := up.Upgrade(WSMuxResponse(w), r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					accepted <- conn
				})
				newServer := httptest.NewServer
				if secure {
					newServer = httptest.NewTLSServer
				}
				srv := newServer(handler)
				defer srv.Close()
				dialer := websocket.Dialer{WriteBufferSize: wsWriteBufferSize, WriteBufferPool: wsWriteBuffers}
				if secure {
					dialer.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
				}
				client, _, err := dialer.Dial("ws"+srv.URL[4:], nil)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				server := <-accepted
				defer server.Close()
				writer, reader := client, server
				if side == "server" {
					writer, reader = server, client
				}
				writer.SetWriteDeadline(time.Now().Add(3 * time.Second))
				reader.SetReadDeadline(time.Now().Add(3 * time.Second))
				stream := WebSocketStream(writer).(*webSocketStream)
				header := []byte{2, 0, 0, 0, 1, 0, 0, 0}
				original := bytes.Clone(header)
				for _, payload := range [][]byte{nil, {}, []byte("ordinary payload"), nil} {
					want := append(bytes.Clone(header), payload...)
					if n, err := stream.WriteBuffers([][]byte{header, payload}); n != len(want) || err != nil {
						t.Fatalf("write %d: %v", n, err)
					}
					kind, got, err := reader.ReadMessage()
					if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, want) {
						t.Fatalf("changed frame: kind %d, bytes %x, error %v", kind, got, err)
					}
					if !bytes.Equal(header, original) {
						t.Fatal("framing changed the caller's control header")
					}
				}
				// Alternate UPD metadata and an application payload of the
				// same size. Reusing control storage must change neither one.
				for i := range 3 {
					h := []byte{2, 4, 8, 0, 1, 0, 0, 0}
					if i == 1 {
						h[1] = smuxPSH
					}
					body := []byte{byte(i + 1), 0, 0, 0, 0, 64, 0, 0}
					hBefore, bodyBefore := bytes.Clone(h), bytes.Clone(body)
					want := append(bytes.Clone(h), body...)
					if n, err := stream.WriteBuffers([][]byte{h, body}); n != len(want) || err != nil {
						t.Fatalf("update/data write %d: %v", n, err)
					}
					kind, got, err := reader.ReadMessage()
					if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, want) {
						t.Fatalf("update/data frame changed: %x, %v", got, err)
					}
					if !bytes.Equal(h, hBefore) || !bytes.Equal(body, bodyBefore) {
						t.Fatal("control staging changed caller-owned metadata or application data")
					}
				}
			})
		}
	}
}
