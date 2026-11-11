package network

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type wsControlSink struct{ net.Conn }

func (*wsControlSink) Write(p []byte) (int, error)      { return len(p), nil }
func (*wsControlSink) Read([]byte) (int, error)         { return 0, io.EOF }
func (*wsControlSink) SetWriteDeadline(time.Time) error { return nil }
func (*wsControlSink) SetReadDeadline(time.Time) error  { return nil }
func (*wsControlSink) SetDeadline(time.Time) error      { return nil }
func (*wsControlSink) Close() error                     { return nil }

type wsControlUpgrade struct {
	conn   net.Conn
	header http.Header
}

func (w wsControlUpgrade) Header() http.Header       { return w.header }
func (wsControlUpgrade) Write(p []byte) (int, error) { return len(p), nil }
func (wsControlUpgrade) WriteHeader(int)             {}
func (w wsControlUpgrade) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

// Discard socket writes so this isolates framing allocations and includes no
// concurrent receiver allocations. The real-connection tests cover wire bytes.
func BenchmarkWSMuxHeaderOnlyWrite(b *testing.B) {
	request := httptest.NewRequest(http.MethodGet, "http://localhost/tunnel", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	response := wsControlUpgrade{conn: &wsControlSink{}, header: make(http.Header)}
	up := websocket.Upgrader{WriteBufferSize: wsWriteBufferSize, WriteBufferPool: wsWriteBuffers}
	conn, err := up.Upgrade(WSMuxResponse(response), request, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	stream := WebSocketStream(conn).(*webSocketStream)
	var header [8]byte
	parts := [][]byte{header[:], nil}
	if _, err := stream.WriteBuffers(parts); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if n, err := stream.WriteBuffers(parts); n != len(header) || err != nil {
			b.Fatalf("write %d: %v", n, err)
		}
	}
}
