package network

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

// The mux peer can open a waiting user's stream immediately after Upgrade.
// Put its first raw bytes in the same write as the 101 response: dropping the
// HTTP reader must not silently drop those bytes before smux sees them.
func TestMuxUpgradePreservesDataCoalescedWithTheHTTPResponse(t *testing.T) {
	for _, mode := range []config.TransportType{config.WSMUX, config.WSSMUX} {
		t.Run(string(mode), func(t *testing.T) {
			payload := []byte{2, 0, 0, 0, 3, 0, 0, 0, 'm', 'u', 'x'}
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				accept := sha1.Sum([]byte(r.Header.Get("Sec-Websocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
				rw.Write(payload)
				rw.Flush()
				<-release
			})
			var srv *httptest.Server
			if mode == config.WSSMUX {
				srv = httptest.NewTLSServer(handler)
			} else {
				srv = httptest.NewServer(handler)
			}
			defer srv.Close()
			addr := strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://")
			conn, err := attemptDialWebSocket(context.Background(), nil, addr, "", "/tunnel", 2*time.Second,
				time.Second, true, "upgrade-regression-token", mode, true, 0, 0, 0, trustTestWSS(t, srv))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.NetConn().SetReadDeadline(time.Now().Add(time.Second))
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn.NetConn(), got); err != nil {
				t.Fatalf("first mux bytes disappeared into the HTTP reader: %v", err)
			}
			if string(got) != string(payload) {
				t.Fatalf("corrupt mux prefix: %x", got)
			}
			if stream := conn.NetConn().(*muxUpgradeConn); stream.reader != nil {
				t.Fatal("upgrade buffer remained attached after its tail was consumed")
			}
		})
	}
}

type upgradeBenchmarkConn struct {
	net.Conn
	input bytes.Reader
}

func (c *upgradeBenchmarkConn) Read(p []byte) (int, error) { return c.input.Read(p) }

func BenchmarkMuxUpgradeReader(b *testing.B) {
	response := []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	b.ReportAllocs()
	for b.Loop() {
		var buf [128]byte
		var raw upgradeBenchmarkConn
		raw.input.Reset(response)
		stream := muxUpgradeStream(&raw)
		if n, err := stream.Read(buf[:]); err != nil || n != len(response) {
			b.Fatalf("read %d, %v", n, err)
		}
	}
}
