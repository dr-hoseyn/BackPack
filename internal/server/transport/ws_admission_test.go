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

	"github.com/gorilla/websocket"
)

func TestWebSocketUpgradeCannotEnterAReplacementClientsPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seat clientSeat
	seat.sit(ctx, func() {}, func() {}, func(ctx context.Context, _ func()) { <-ctx.Done() })
	owner := seat.client()
	// The HTTP upgrade is in flight while another authenticated control
	// channel replaces this client and drains its pool.
	seat.sit(ctx, func() {}, func() {}, func(ctx context.Context, _ func()) { <-ctx.Done() })
	queued := false
	if seat.admitFor(ctx, owner, func(context.Context) bool { queued = true; return true }) || queued {
		t.Fatal("a late upgrade from the previous client entered the new pool")
	}
	if !seat.admitFor(ctx, seat.client(), func(context.Context) bool { queued = true; return true }) || !queued {
		t.Fatal("the replacement client could not add capacity")
	}
	cancel()
	if seat.admitFor(ctx, seat.client(), func(context.Context) bool { t.Error("ended generation admitted a pool connection"); return true }) {
		t.Fatal("ended generation accepted an upgrade")
	}
}

func TestWebSocketControlCancelledBeforeSeatingClosesItsSocket(t *testing.T) {
	for _, mux := range []bool{false, true} {
		name := "ws"
		if mux {
			name = "wssmux"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			returned := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() {
				close(release)
				select {
				case <-returned:
				case <-time.After(2 * time.Second):
					t.Error("control fixture did not finish")
				}
			})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(returned)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				if endedClaim(ctx, conn) {
					t.Error("fixture ended before the pre-admission check")
					return
				}
				// Reproduce shutdown in the gap after the handler's check.
				cancel()
				if mux {
					(&WsMuxTransport{}).seatClient(&wsMuxGen{ctx: ctx}, conn)
				} else {
					(&WsTransport{}).seatClient(&wsGen{ctx: ctx}, conn)
				}
				// Keep the handler alive: the candidate must be closed by
				// seatClient, rather than this fixture's deferred cleanup.
				<-release
			}))
			defer srv.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, _, err = conn.ReadMessage()
			var timeout net.Error
			if err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
				t.Fatalf("rejected control candidate was left open: %v", err)
			}
		})
	}
}
