package transport

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

func TestWSSMuxSpreadRecoversDataSessionOnControlRoute(t *testing.T) {
	stalled, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	var stalledAttempts atomic.Int32
	go func() {
		for {
			conn, err := stalled.Accept()
			if err != nil {
				return
			}
			stalledAttempts.Add(1)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _ = io.Copy(io.Discard, conn)
			_ = conn.Close()
		}
	}()
	const token = "data-failover-test-token"
	upgrader := websocket.Upgrader{Subprotocols: []string{network.WSMuxBinaryProtocol}}
	healthy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/tunnel/") {
			http.NotFound(w, r)
			return
		}
		proof, err := network.WSSServerAnswer(r.TLS, token)
		if err != nil {
			http.Error(w, "proof unavailable", http.StatusInternalServerError)
			return
		}
		headers := http.Header{}
		headers.Set(network.WSSServerProofHeader, proof)
		headers.Set(network.WSMuxVersionHeader, "2")
		headers.Set(network.WSMuxPoolHeader, "2")
		if conn, err := upgrader.Upgrade(w, r, headers); err == nil {
			conn.Close()
		}
	}))
	defer healthy.Close()

	controlAddr := healthy.Listener.Addr().String()
	endpoints := network.NewEndpoints(stalled.Addr().String(), controlAddr)
	endpoints.SetSpread(true)
	endpoints.Prefer(controlAddr)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	cfg := smux.DefaultConfig()
	client := NewWSMuxClient(ctx, &WsMuxConfig{
		Endpoints: endpoints, Token: token, Mode: config.WSSMUX, DialTimeOut: 500 * time.Millisecond,
		MaxFrameSize: cfg.MaxFrameSize, MaxReceiveBuffer: cfg.MaxReceiveBuffer, MaxStreamBuffer: cfg.MaxStreamBuffer,
	}, logrus.New())
	mux := network.WSMuxOptions{PoolSize: 2}
	start := time.Now()
	conn, err := client.dialTunnel(ctx, &mux)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Logf("authenticated data session established in %v", time.Since(start))
	if got := conn.RemoteAddr().String(); got != controlAddr {
		t.Fatalf("data session connected to %s, want %s", got, controlAddr)
	}
	if endpoints.Current() != controlAddr || mux.NegotiatedVersion != 2 || !mux.Retirement ||
		conn.Subprotocol() != network.WSMuxBinaryProtocol {
		t.Fatalf("control preference or negotiated session options lost: %s, %+v", endpoints.Current(), mux)
	}
	// The fixture closes after upgrading. This exercises the actual session
	// exit hook as well as the handshake, without a running pool reconciler.
	client.poolConnections = 1
	client.handleSession(conn, mux)
	select {
	case <-client.poolLost:
		if client.poolConnections != 0 {
			t.Fatal("repair was notified before returning the open slot")
		}
	default:
		t.Fatal("closed data session did not notify the pool")
	}
	// Round robin starts the third dial on the failed route again. It
	// should go straight to the working route while the failure is fresh.
	for range 2 {
		replacement, err := client.dialTunnel(ctx, &mux)
		if err != nil {
			t.Fatal(err)
		}
		replacement.Close()
	}
	if got := stalledAttempts.Load(); got != 1 {
		t.Fatalf("pool repair retried the recently failed route: %d attempts", got)
	}
	// Both routes now have a recent failure. The next round-robin route
	// must still get a chance to reconnect.
	client.dataRoutes.record(controlAddr, time.Now(), false)
	replacement, err := client.dialTunnel(ctx, &mux)
	if err != nil {
		t.Fatalf("all routes cooling down prevented a recovery probe: %v", err)
	}
	replacement.Close()
}

func TestWSMuxRouteCooldownExpiresAndIgnoresAnOlderFailure(t *testing.T) {
	var routes wsMuxDataRoutes
	now := time.Now()
	routes.record("peer", now, false)
	if routes.available("peer", time.Now()) || !routes.available("peer", now.Add(3*wsMuxDataRetryDelay)) {
		t.Fatal("failed route did not enter or leave cooldown")
	}
	routes.record("peer", now.Add(time.Millisecond), true)
	routes.record("peer", now, false)
	if !routes.available("peer", time.Now()) {
		t.Fatal("an older attempt suppressed a route that already recovered")
	}
}

func TestWSSMuxControlFallsBackAfterOneStalledHandshake(t *testing.T) {
	// TCP connects but the primary never replies to the TLS ClientHello.
	primary, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()

	const token = "failover-test-token"
	upgrader := websocket.Upgrader{}
	backup := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channel" {
			http.NotFound(w, r)
			return
		}
		proof, err := network.WSSServerAnswer(r.TLS, token)
		if err != nil {
			http.Error(w, "proof unavailable", http.StatusInternalServerError)
			return
		}
		headers := http.Header{}
		headers.Set(network.WSSServerProofHeader, proof)
		if conn, err := upgrader.Upgrade(w, r, headers); err == nil {
			conn.Close()
		}
	}))
	defer backup.Close()

	backupAddr := backup.Listener.Addr().String()
	endpoints := network.NewEndpoints(primary.Addr().String(), backupAddr)
	client := &WsMuxTransport{
		config: &WsMuxConfig{
			Endpoints:   endpoints,
			Token:       token,
			Mode:        config.WSSMUX,
			DialTimeOut: 500 * time.Millisecond,
		},
	}
	client.logger = logrus.New()

	// This budget allows one failed handshake and a healthy local upgrade,
	// but expires before the old three attempts and their retry waits finish.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := client.dialControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Logf("authenticated fallback established in %v", time.Since(start))
	if got := conn.RemoteAddr().String(); got != backupAddr {
		t.Fatalf("control connected to %s, want %s", got, backupAddr)
	}
	if got := endpoints.Next(); got != backupAddr {
		t.Fatalf("data pool would dial %s, want %s", got, backupAddr)
	}
}
