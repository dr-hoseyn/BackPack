package naive

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"github.com/backpack/backpack/config"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// The controlled STUN server reports the actual loopback UDP endpoint. This
// proves rendezvous and hole-punch coordination locally, not arbitrary NATs.
func realmTestSTUN(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			if n < 20 || binary.BigEndian.Uint16(b[:2]) != 1 || binary.BigEndian.Uint32(b[4:8]) != 0x2112a442 {
				continue
			}
			a := from.(*net.UDPAddr)
			ip := a.IP.To4()
			if ip == nil {
				continue
			}
			reply := make([]byte, 32)
			binary.BigEndian.PutUint16(reply[:2], 0x0101)
			binary.BigEndian.PutUint16(reply[2:4], 12)
			copy(reply[4:20], b[4:20])
			binary.BigEndian.PutUint16(reply[20:22], 0x0020)
			binary.BigEndian.PutUint16(reply[22:24], 8)
			reply[25] = 1
			binary.BigEndian.PutUint16(reply[26:28], uint16(a.Port)^0x2112)
			for i := 0; i < 4; i++ {
				reply[28+i] = ip[i] ^ b[4+i]
			}
			_, _ = pc.WriteTo(reply, from)
		}
	}()
	return pc.LocalAddr().String()
}

func TestSingRealmOfficialServiceAndDiscoveredPayload(t *testing.T) {
	bin := os.Getenv("BP_SINGBOX_BINARY")
	if bin == "" {
		t.Skip("set BP_SINGBOX_BINARY to pinned sing-box")
	}
	s, c := singFixture(t, "hysteria2")
	s.Sing.Binary, c.Sing.Binary = bin, bin
	t.Setenv("SSL_CERT_FILE", c.Sing.CAFile)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	log := logrus.New()
	log.SetOutput(io.Discard)
	realmAddr := singFreeAddr(t, false)
	ready := singFreeAddr(t, false)
	rc := RealmServerConfig{Binary: bin, Listen: realmAddr, ServerName: s.Sing.ServerName, Certificate: s.Sing.Certificate, Key: s.Sing.Key, Token: "independent-realm-account-token", MaxRealms: 1}
	body, err := RealmServerJSON(rc, ready)
	if err != nil {
		t.Fatal(err)
	}
	h, err := startManaged(ctx, bin, "sing-realm", body, ready, os.Environ(), log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	root := x509.NewCertPool()
	cert, err := os.ReadFile(c.Sing.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	root.AppendCertsFromPEM(cert)
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, ServerName: s.Sing.ServerName, RootCAs: root}}}
	defer client.CloseIdleConnections()
	post := func(token string) int {
		req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+realmAddr+"/v1/auth-check/", bytes.NewBufferString(`{"addresses":["127.0.0.1:9999"]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rsp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, rsp.Body)
		rsp.Body.Close()
		return rsp.StatusCode
	}
	if code := post("incorrect-realm-account-token"); code != http.StatusUnauthorized {
		t.Fatalf("wrong Realm token: %d", code)
	}
	// Do not occupy the one registration slot; the actual inbound claims it.
	s.Sing.Listen = singFreeAddr(t, true)
	_, realmPort, _ := net.SplitHostPort(realmAddr)
	r := config.HysteriaRealm{ServerURL: "https://" + net.JoinHostPort(s.Sing.ServerName, realmPort), RealmID: "local-discovery", Token: rc.Token, STUNServers: realmTestSTUN(t), IPVersion: 4}
	s.Sing.Realm, c.Sing.Realm = r, r
	c.Sing.Server = "127.0.0.1:1" // a direct endpoint cannot accidentally pass.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	s.BindAddr, c.RemoteAddr = backend.Addr().String(), backend.Addr().String()
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	server, err := StartSingServer(ctx, &s, log)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	outside, err := StartSingClient(ctx, &c, log)
	if err != nil {
		t.Fatal(err)
	}
	defer outside.Close()
	d, err := proxy.SOCKS5("tcp", outside.addr, nil, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	var conn net.Conn
	for attempt := 0; attempt < 10; attempt++ {
		dialCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		conn, err = d.(proxy.ContextDialer).DialContext(dialCtx, "tcp", c.RemoteAddr)
		stop()
		if err == nil {
			break
		}
		if !sleep(ctx, 200*time.Millisecond) {
			break
		}
	}
	if err != nil {
		t.Fatalf("Realm did not discover a usable peer: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	want := []byte("verified through the actual Realm-discovered peer")
	if _, err := conn.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Realm payload: %v", err)
	}
	if code := post(rc.Token); code != http.StatusTooManyRequests {
		t.Fatalf("registration quota was not enforced: %d", code)
	}
}
