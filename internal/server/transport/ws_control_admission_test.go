package transport

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/gorilla/websocket"
)

func TestWebSocketControlRejectedDuringShutdownClosesItsSocket(t *testing.T) {
	for _, mode := range []config.TransportType{config.WS, config.WSS, config.WSMUX, config.WSSMUX} {
		t.Run(string(mode), func(t *testing.T) {
			accepted := make(chan *websocket.Conn, 1)
			upgrader := websocket.Upgrader{}
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				accepted <- conn
			}))
			secure := mode == config.WSS || mode == config.WSSMUX
			if secure {
				srv.StartTLS()
			} else {
				srv.Start()
			}
			defer srv.Close()
			dialer := websocket.Dialer{HandshakeTimeout: time.Second}
			if secure {
				dialer.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			}
			client, _, err := dialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server := <-accepted
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// Model cancellation after the HTTP handler's endedClaim check
			// but before the seat admits the upgraded control connection.
			if endedClaim(ctx, server) {
				t.Fatal("live generation rejected upgrade")
			}
			cancel()
			if mode == config.WSMUX || mode == config.WSSMUX {
				s := &WsMuxTransport{}
				if s.seatClient(&wsMuxGen{ctx: ctx}, server) {
					t.Fatal("rejected mux claim reported successful admission")
				}
				if s.controlChannel.IsSet() {
					t.Fatal("ended mux generation installed a control channel")
				}
			} else {
				s := &WsTransport{}
				if s.seatClient(&wsGen{ctx: ctx}, server) {
					t.Fatal("rejected claim reported successful admission")
				}
				if s.controlChannel.IsSet() {
					t.Fatal("ended generation installed a control channel")
				}
			}
			client.SetReadDeadline(time.Now().Add(time.Second))
			_, _, err = client.ReadMessage()
			var timeout net.Error
			if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
				t.Fatalf("rejected hijacked socket remained open: %v", err)
			}
		})
	}
}
