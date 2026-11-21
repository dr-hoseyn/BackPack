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
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
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
		Endpoints: endpoints, Token: token, Mode: config.WSSMUX, DialTimeOut: 10 * time.Second,
		MaxFrameSize: cfg.MaxFrameSize, MaxReceiveBuffer: cfg.MaxReceiveBuffer, MaxStreamBuffer: cfg.MaxStreamBuffer,
	}, logrus.New())
	mux := network.WSMuxOptions{PoolSize: 2}
	start := time.Now()
	conn, err := client.dialTunnel(ctx, &mux)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("fallback consumed the user's admission budget: %v", elapsed)
	}
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

func TestWSSMuxControlFallsBackWithinOneBudgetAfterTwoStalledHandshakes(t *testing.T) {
	// TCP connects but the primary never replies to the TLS ClientHello.
	primary, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	second, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

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
	endpoints := network.NewEndpoints(primary.Addr().String(), second.Addr().String(), backupAddr)
	client := &WsMuxTransport{
		config: &WsMuxConfig{
			Endpoints:   endpoints,
			Token:       token,
			Mode:        config.WSSMUX,
			DialTimeOut: 500 * time.Millisecond,
		},
	}
	client.logger = logrus.New()

	// Two stalled attempts must leave budget for the healthy third route.
	// A parent deadline longer than the round keeps the budgets distinct.
	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
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

func TestWSSMuxOrdinaryDataFallbackKeepsControlOnPrimary(t *testing.T) {
	const token = "ordinary-data-fallback-test-token"
	upgrader := websocket.Upgrader{Subprotocols: []string{network.WSMuxBinaryProtocol}}
	var rejectedData atomic.Int32
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channel" {
			rejectedData.Add(1)
			http.Error(w, "data route unavailable", http.StatusServiceUnavailable)
			return
		}
		proof, err := network.WSSServerAnswer(r.TLS, token)
		if err != nil {
			t.Error(err)
			return
		}
		headers := http.Header{network.WSSServerProofHeader: []string{proof}}
		conn, err := upgrader.Upgrade(w, r, headers)
		if err != nil {
			return
		}
		defer conn.Close()
		kind, body, err := conn.ReadMessage()
		if err == nil {
			_ = conn.WriteMessage(kind, body)
		}
	}))
	defer primary.Close()

	backup := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/tunnel/") {
			t.Errorf("unexpected backup control request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		proof, err := network.WSSServerAnswer(r.TLS, token)
		if err != nil {
			t.Error(err)
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
	defer backup.Close()

	primaryAddr, backupAddr := primary.Listener.Addr().String(), backup.Listener.Addr().String()
	endpoints := network.NewEndpoints(primaryAddr, backupAddr)
	client := &WsMuxTransport{config: &WsMuxConfig{
		Endpoints: endpoints, Token: token, Mode: config.WSSMUX, DialTimeOut: 500 * time.Millisecond,
	}}
	client.logger = logrus.New()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	control, err := client.dialControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	for range 2 {
		mux := network.WSMuxOptions{Version: 2, PoolSize: 2}
		data, err := client.dialTunnel(ctx, &mux)
		if err != nil {
			t.Fatal(err)
		}
		_ = data.Close()
		if got := data.RemoteAddr().String(); got != backupAddr || !mux.Retirement || mux.NegotiatedVersion != 2 {
			t.Fatalf("fallback data route = %s, negotiated options = %+v", got, mux)
		}
	}
	if got := rejectedData.Load(); got != 1 {
		t.Fatalf("primary data route was tried %d times, want 1", got)
	}
	if control.RemoteAddr().String() != primaryAddr || endpoints.Current() != primaryAddr {
		t.Fatal("data fallback moved the control route")
	}
	_ = control.SetReadDeadline(time.Now().Add(time.Second))
	if err := control.WriteMessage(websocket.BinaryMessage, []byte("control still connected")); err != nil {
		t.Fatal(err)
	}
	if _, reply, err := control.ReadMessage(); err != nil || string(reply) != "control still connected" {
		t.Fatalf("original control connection reply = %q, %v", reply, err)
	}

	t.Run("later backup remains reachable when cooldowns expire", func(t *testing.T) {
		refuse := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "route unavailable", http.StatusServiceUnavailable)
		})
		a, b := httptest.NewTLSServer(refuse), httptest.NewTLSServer(refuse)
		defer a.Close()
		defer b.Close()
		endpoints := network.NewEndpoints(primaryAddr, a.Listener.Addr().String(), b.Listener.Addr().String(), backupAddr)
		client := &WsMuxTransport{config: &WsMuxConfig{
			Endpoints: endpoints, Token: token, Mode: config.WSSMUX, DialTimeOut: 500 * time.Millisecond,
		}}
		client.logger = logrus.New()
		mux := network.WSMuxOptions{Version: 2, PoolSize: 2}
		first, err := client.dialTunnel(t.Context(), &mux)
		if first != nil {
			_ = first.Close()
			t.Fatal("first dial exceeded its three-attempt budget")
		}
		if err == nil {
			t.Fatal("failed dial returned no error")
		}
		// Emulate the cooldown expiring between dials while retaining the
		// fallback cursor, as happens when early alternatives fail slowly.
		client.dataRoutes.mu.Lock()
		for addr, state := range client.dataRoutes.routes {
			state.retryAt = time.Time{}
			client.dataRoutes.routes[addr] = state
		}
		client.dataRoutes.mu.Unlock()

		data, err := client.dialTunnel(t.Context(), &mux)
		if err != nil {
			t.Fatal(err)
		}
		defer data.Close()
		if got := data.RemoteAddr().String(); got != backupAddr {
			t.Fatalf("connected to %s, want last backup %s", got, backupAddr)
		}
	})
}

func TestWSSMuxCapacityDoesNotCountAsRouteFailure(t *testing.T) {
	var routes wsMuxDataRoutes
	now := time.Now()
	routes.recordResult("full", now, network.ErrWSMuxCapacity)
	routes.recordResult("backup", now.Add(-1500*time.Millisecond), nil)
	if routes.available("full", now) || !routes.atCapacity("full", now) {
		t.Fatal("route with no capacity must cool down")
	}
	if got := routes.quickBudget("backup", 800*time.Millisecond, 5*time.Second); got < 2*time.Second || got > 3*time.Second {
		t.Fatalf("known slow route got a %v handshake budget", got)
	}
	stats := routes.snapshot([]string{"full"})
	if len(stats) != 1 || stats[0].CapacityRejected != 1 || stats[0].Failures != 0 {
		t.Fatalf("capacity response polluted reachability statistics: %+v", stats)
	}
}
