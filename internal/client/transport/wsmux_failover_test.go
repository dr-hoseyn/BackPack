package transport

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

func TestWSSMuxSpreadRecoversDataSessionOnControlRoute(t *testing.T) {
	stalled, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
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
	client := &WsMuxTransport{config: &WsMuxConfig{
		Endpoints: endpoints, Token: token, Mode: config.WSSMUX, DialTimeOut: 500 * time.Millisecond,
	}}
	client.logger = logrus.New()
	mux := network.WSMuxOptions{PoolSize: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
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
