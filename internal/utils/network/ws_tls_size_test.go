package network

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type wsTLSRecordConn struct {
	net.Conn
	records atomic.Int64
}

func (c *wsTLSRecordConn) Write(p []byte) (int, error) {
	// Go TLS emits one record per raw Write. Handshake/upgrade writes are
	// excluded by taking the counter only after the WebSocket is ready.
	if len(p) >= 5 && p[0] == 23 {
		c.records.Add(1)
	}
	return c.Conn.Write(p)
}

type wsTLSRecordListener struct {
	net.Listener
	accepted chan *wsTLSRecordConn
}

func (l *wsTLSRecordListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	wrapped := &wsTLSRecordConn{Conn: c}
	l.accepted <- wrapped
	return wrapped, nil
}

func TestWebSocketTLSFramesAvoidTinyTrailingRecords(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{WriteBufferSize: 64 << 10}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		accepted <- ws
	}))
	listener := &wsTLSRecordListener{Listener: srv.Listener, accepted: make(chan *wsTLSRecordConn, 1)}
	srv.Listener = listener
	// Make record boundaries deterministic. Production retains its existing
	// TLS policy, including adaptive record sizes early in a connection.
	srv.TLS = &tls.Config{DynamicRecordSizingDisabled: true}
	srv.StartTLS()
	defer srv.Close()
	clientTLS := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	clientTLS.DynamicRecordSizingDisabled = true
	var clientRaw *wsTLSRecordConn
	dialer := websocket.Dialer{
		TLSClientConfig: clientTLS,
		WriteBufferSize: 64 << 10,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			clientRaw = &wsTLSRecordConn{Conn: c}
			return clientRaw, nil
		},
	}
	client, _, err := dialer.Dial("wss"+strings.TrimPrefix(srv.URL, "https"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	serverRaw := <-listener.accepted
	for _, side := range []string{"server", "client"} {
		writer, reader, raw := server, client, serverRaw
		if side == "client" {
			writer, reader, raw = client, server, clientRaw
		}
		for _, tc := range []struct {
			name  string
			size  int
			muxed bool
		}{{"WSS/16KiB", 16384, false}, {"WSS/64KiB", 65536, false}, {"WSSMux/16KiB", 16384, true}, {"WSSMux/32KiB", 32768, true}, {"WSSMux/max", 65535, true}} {
			t.Run(side+"/"+tc.name, func(t *testing.T) {
				var counts []int64
				aligned := WebSocketTLSReadSize(tc.size)
				if tc.muxed {
					aligned = WSMuxFrameSize(tc.size, true)
				}
				for _, size := range []int{tc.size, aligned} {
					_ = writer.SetWriteDeadline(time.Now().Add(3 * time.Second))
					_ = reader.SetReadDeadline(time.Now().Add(3 * time.Second))
					payload := bytes.Repeat([]byte("x"), size)
					want := payload
					before := raw.records.Load()
					if tc.muxed {
						header := []byte{2, 2, 0, 0, 1, 0, 0, 0}
						binary.LittleEndian.PutUint16(header[2:], uint16(size))
						want = append(bytes.Clone(header), payload...)
						if n, err := WebSocketStream(writer).(*webSocketStream).WriteBuffers([][]byte{header, payload}); err != nil || n != len(want) {
							t.Fatalf("mux write: n=%d err=%v", n, err)
						}
					} else if err := writer.WriteMessage(websocket.BinaryMessage, payload); err != nil {
						t.Fatal(err)
					}
					kind, got, err := reader.ReadMessage()
					if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, want) {
						t.Fatalf("TLS framing changed data: n=%d want=%d err=%v", len(got), len(want), err)
					}
					counts = append(counts, raw.records.Load()-before)
				}
				if counts[1] >= counts[0] || counts[1] != int64((aligned+tlsRecordPlaintextSize-1)/tlsRecordPlaintextSize) {
					t.Fatalf("tiny record was not removed: sizes=%d/%d records=%v", tc.size, aligned, counts)
				}
				t.Logf("payload %d -> %d; TLS records %d -> %d", tc.size, aligned, counts[0], counts[1])
			})
		}
	}
}

func TestWebSocketTLSSizePreservesSmallAndCustomLimits(t *testing.T) {
	for _, size := range []int{0, 64, 4096, 16000, 20000, 32000, 60000, 65537} {
		if got := WSMuxFrameSize(size, true); got != size {
			t.Fatalf("custom frame size changed: %d -> %d", size, got)
		}
	}
	for _, size := range []int{16384, 32768, 65535} {
		if got := WSMuxFrameSize(size, false); got != size {
			t.Fatal("plain WS acquired TLS sizing")
		}
	}
}
