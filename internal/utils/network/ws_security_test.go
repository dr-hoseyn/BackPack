package network

import (
	"context"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/gorilla/websocket"
)

func trustTestWSS(t *testing.T, srv *httptest.Server) *WSSClientOptions {
	t.Helper()
	o := &WSSClientOptions{}
	if srv.TLS == nil {
		return o
	}
	o.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(o.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.TLS.Certificates[0].Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestWSSBearerTokenRequiresVerifiedCertificate(t *testing.T) {
	var requests atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer private-test-token" {
			t.Error("missing bearer token")
		}
		c, err := up.Upgrade(w, r, nil)
		if err == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	dial := func(o *WSSClientOptions) error {
		c, err := WebSocketDialer(context.Background(), nil, srv.Listener.Addr().String(), "", "/channel", time.Second, time.Second, true, "private-test-token", config.WSSMUX, true, 1, 0, 0, 0, o)
		if c != nil {
			c.Close()
		}
		return err
	}
	if err := dial(nil); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("credential sent before authenticating TLS")
	}
	trusted := trustTestWSS(t, srv)
	if err := dial(trusted); err != nil {
		t.Fatalf("trusted private CA: %v", err)
	}
	wrong := trustTestWSS(t, srv)
	wrong.ServerName = "wrong.example"
	if err := dial(wrong); err == nil {
		t.Fatal("wrong hostname accepted")
	}
	if requests.Load() != 1 {
		t.Fatal("credential sent to wrong hostname")
	}
}

func TestWSSLegacyModeRequiresVerifiedTLS(t *testing.T) {
	var requests atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		c, e := up.Upgrade(w, r, nil)
		if e == nil {
			c.Close()
		}
	}))
	defer srv.Close()
	dial := func(o *WSSClientOptions) error {
		c, e := WebSocketDialer(context.Background(), nil, srv.Listener.Addr().String(), "", "/channel", time.Second, time.Second, true, "test-token", config.WSS, false, 1, 0, 0, 0, o)
		if c != nil {
			c.Close()
		}
		return e
	}
	if e := dial(&WSSClientOptions{AllowLegacyServer: true}); e == nil {
		t.Fatal("legacy mode bypassed TLS identity")
	}
	if requests.Load() != 0 {
		t.Fatal("HTTP sent before authenticating legacy server")
	}
	o := trustTestWSS(t, srv)
	o.AllowLegacyServer = true
	if e := dial(o); e != nil {
		t.Fatal(e)
	}
}

func TestWSSCachesH2FallbackWithoutChangingDirectOffer(t *testing.T) {
	var connections atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proof, e := WSSServerAnswer(r.TLS, "token")
		if e != nil {
			t.Error(e)
			return
		}
		c, e := up.Upgrade(w, r, http.Header{WSSServerProofHeader: {proof}})
		if e == nil {
			c.Close()
		}
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "https://")
	key := addr + "\x00" + addr + "\x00"
	wssALPN.forget(key)
	defer wssALPN.forget(key)
	for i := 0; i < 2; i++ {
		c, e := WebSocketDialer(context.Background(), nil, addr, "", "/channel", time.Second, time.Second, true, "token", config.WSS, false, 1, 0, 0, 0)
		if e != nil {
			t.Fatal(e)
		}
		c.Close()
	}
	if n := connections.Load(); n != 3 {
		t.Fatalf("two upgrades used %d TLS connections, want 3", n)
	}
	var cache alpnCapabilityCache
	now := time.Now()
	cache.remember("peer", now)
	if !cache.http11("peer", now) || cache.http11("other", now) || cache.http11("peer", now.Add(wssALPNTTL)) {
		t.Fatal("cache isolation/expiration")
	}
	for i := 0; i < wssALPNEntries+10; i++ {
		cache.remember(time.Unix(int64(i), 0).String(), now)
	}
	if len(cache.entries) > wssALPNEntries {
		t.Fatal("unbounded cache")
	}
}

func TestWSSUnprovenPeerCannotPopulateALPNCache(t *testing.T) {
	var requests atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		c, e := up.Upgrade(w, r, nil)
		if e == nil {
			c.Close()
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	key := addr + "\x00" + addr + "\x00"
	wssALPN.forget(key)
	defer wssALPN.forget(key)
	c, e := WebSocketDialer(context.Background(), nil, addr, "", "/channel", time.Second, time.Second, true, "secret-not-known-to-peer", config.WSS, false, 3, 0, 0, 0)
	if c != nil {
		c.Close()
	}
	if e == nil {
		t.Fatal("unproven peer accepted")
	}
	if requests.Load() != 1 {
		t.Fatal("same unproven peer was retried inside the dial attempt")
	}
	if wssALPN.http11(key, time.Now()) {
		t.Fatal("unauthenticated peer changed future ClientHello behavior")
	}
}

func TestWSSExplicitHTTP11ProfileSkipsH2Probe(t *testing.T) {
	var connections atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User-Id") != "" || !strings.Contains(r.UserAgent(), "Chrome/133.") {
			t.Error("inconsistent HTTP identity")
		}
		proof, e := WSSServerAnswer(r.TLS, "token")
		if e != nil {
			t.Error(e)
			return
		}
		c, e := up.Upgrade(w, r, http.Header{WSSServerProofHeader: {proof}})
		if e == nil {
			c.Close()
		}
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			connections.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()
	for i := 0; i < 2; i++ {
		c, e := WebSocketDialer(context.Background(), nil, srv.Listener.Addr().String(), "", "/channel", time.Second, time.Second, true, "token", config.WSSMUX, false, 1, 0, 0, 0, &WSSClientOptions{HTTP11Only: true})
		if e != nil {
			t.Fatal(e)
		}
		c.Close()
	}
	if connections.Load() != 2 {
		t.Fatal("explicit WebSocket profile made an unnecessary h2 connection")
	}
}
