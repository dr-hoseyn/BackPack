package e2e

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/gorilla/websocket"
)

func TestWSSHealthProbesPreserveControlAndForwardedStream(t *testing.T) {
	for _, mode := range []config.TransportType{config.WSS, config.WSSMUX} {
		t.Run(string(mode), func(t *testing.T) {
			const token = "websocket-health-test-0123456789abcdef"
			backend := startEchoBackend(t)
			control, entry := freePort(t), freePort(t)
			srv := baseServerConfig(string(mode), control, entry, backend.addr, token)
			srv.TLSCertFile, srv.TLSKeyFile = testCert(t)
			tun := runPair(t, srv, baseClientConfig(string(mode), srv.BindAddr, token, nil), entry, control)
			if err := tun.waitReady(tunnelReadyTimeout); err != nil {
				t.Fatal(err)
			}
			conn, err := net.DialTimeout("tcp", tun.Entry, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			expectHealthTestEcho(t, conn, "before health probes")
			probe := network.WebSocketHealthProbe(nil, "", token, mode, false, 0, nil)
			if s := probe(t.Context(), srv.BindAddr, 3); !s.Reachable || s.LossPct != 0 {
				t.Fatalf("valid probe failed: %+v", s)
			}
			wrong := network.WebSocketHealthProbe(nil, "", "wrong-token", mode, false, 0, nil)
			if s := wrong(t.Context(), srv.BindAddr, 1); s.Reachable {
				t.Fatalf("wrong credential passed the probe: %+v", s)
			}
			expectHealthTestEcho(t, conn, "after health probes")
			if err := tun.roundTrip([]byte("new user after probes")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWSSHealthSteeringMovesNewTrafficWithoutReplacingAnActiveStream(t *testing.T) {
	if !ipv6Available(t) {
		t.Skip("requires IPv6 loopback")
	}
	backend := startEchoBackend(t)
	control, entry := freePort(t), freePort(t)
	srv := baseServerConfig("wss", control, entry, backend.addr, "wss-steering-0123456789abcdef")
	srv.TLSCertFile, srv.TLSKeyFile = testCert(t)
	v4 := newControlledPathOn(t, srv.BindAddr, "tcp4", "127.0.0.1:0")
	v6 := newControlledPathOn(t, srv.BindAddr, "tcp6", "[::1]:0")
	v6.delay.Store(int64(200 * time.Millisecond))
	cli := baseClientConfig("wss", v4.addr, srv.Token, []string{v6.addr})
	cli.HealthFailover = true
	tun := runPair(t, srv, cli, entry, control)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", tun.Entry, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	expectHealthTestEcho(t, conn, "before IPv6 scores better")

	// Delay only new IPv4 handshakes, well within the dial timeout. Both
	// routes remain usable and the established IPv4 sockets keep working.
	v4.delay.Store(int64(200 * time.Millisecond))
	v6.delay.Store(0)
	waitAddressTrafficAfter(t, tun, v6, v6.bytes.Load(), 35*time.Second)
	expectHealthTestEcho(t, conn, "after new data dials move to IPv6")
}

func expectHealthTestEcho(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, []byte(payload)) {
		t.Fatalf("existing stream failed to echo: %v", err)
	}
}

func TestWSSHealthProbeRejectsAnUnrelatedUpgrade(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			conn.Close()
		}
	}))
	defer server.Close()
	probe := network.WebSocketHealthProbe(nil, "", "test-token", config.WS, false, 0, nil)
	if s := probe(t.Context(), server.Listener.Addr().String(), 1); s.Reachable {
		t.Fatalf("a generic WebSocket upgrade passed the tunnel health check: %+v", s)
	}
}
