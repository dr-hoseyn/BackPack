package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils/network"
	"github.com/gorilla/websocket"
)

// A message-aware proxy cannot relay the previous raw smux format. Both legs
// parse and reserialize real frames, including masked client frames, so a
// successful application echo proves more than a 101 followed by raw bytes.
func TestWSMuxCarriesTrafficThroughAFramingProxy(t *testing.T) {
	backend := startEchoBackend(t)
	tunnelPort, entryPort := freePort(t), freePort(t)
	const token = "framing-proxy-test-token"
	var dataMessages atomic.Int32
	up := websocket.Upgrader{Subprotocols: []string{network.WSMuxBinaryProtocol}, CheckOrigin: func(*http.Request) bool { return true }}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := http.Header{}
		for _, name := range []string{"Authorization", network.WSMuxOfferHeader, network.WSMuxPoolHeader} {
			if value := r.Header.Get(name); value != "" {
				h.Set(name, value)
			}
		}
		d := websocket.Dialer{Subprotocols: websocket.Subprotocols(r), HandshakeTimeout: 2 * time.Second}
		origin, resp, err := d.Dial(fmt.Sprintf("ws://127.0.0.1:%d%s", tunnelPort, r.URL.Path), h)
		if err != nil {
			http.Error(w, "origin unavailable", http.StatusBadGateway)
			return
		}
		defer origin.Close()
		answer := http.Header{}
		for _, name := range []string{network.WSMuxVersionHeader, network.WSMuxPoolHeader} {
			if value := resp.Header.Get(name); value != "" {
				answer.Set(name, value)
			}
		}
		peer, err := up.Upgrade(w, r, answer)
		if err != nil {
			return
		}
		defer peer.Close()
		done := make(chan struct{}, 2)
		copyMessages := func(to, from *websocket.Conn) {
			defer func() { done <- struct{}{} }()
			for {
				kind, payload, e := from.ReadMessage()
				if e != nil {
					return
				}
				if strings.HasPrefix(r.URL.Path, "/tunnel") {
					dataMessages.Add(1)
				}
				if e = to.WriteMessage(kind, payload); e != nil {
					return
				}
			}
		}
		go copyMessages(origin, peer)
		go copyMessages(peer, origin)
		<-done
		origin.Close()
		peer.Close()
		<-done
	}))
	defer proxy.Close()
	s := baseServerConfig("wsmux", tunnelPort, entryPort, backend.addr, token)
	c := baseClientConfig("wsmux", proxy.Listener.Addr().String(), token, nil)
	tun := runPair(t, s, c, entryPort, tunnelPort)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := tun.roundTrip(randomPayload(t, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if dataMessages.Load() < 2 {
		t.Fatal("proxy did not parse bidirectional data frames")
	}
}
