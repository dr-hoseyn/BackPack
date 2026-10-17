package naive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

func shortFixture(t *testing.T, target string) (config.ServerConfig, config.ClientConfig) {
	t.Helper()
	ts := httptest.NewTLSServer(nil)
	pair := ts.TLS.Certificates[0]
	ts.Close()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	priv, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}), 0600); err != nil {
		t.Fatal(err)
	}
	s := config.ServerConfig{Transport: config.TCP, BindAddr: target, Token: "a-long-random-reverse-token-for-tests", Short: config.ShortServerConfig{Listen: singFreeAddr(t, false), ServerName: "127.0.0.1", Certificate: cert, Key: key}}
	c := config.ClientConfig{Transport: config.TCP, RemoteAddr: target, Token: s.Token, Short: config.ShortClientConfig{Server: s.Short.Listen, CAFile: cert}}
	return s, c
}

func shortConnect(t *testing.T, helper *Helper, target string) *net.TCPConn {
	t.Helper()
	u, err := url.Parse(helper.ProxyURL())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := raw.(*net.TCPConn)
	c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		c.Close()
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	line, err := r.ReadString('\n')
	if err != nil || line != "HTTP/1.1 200 Connection Established\r\n" {
		c.Close()
		t.Fatalf("CONNECT: %q %v", line, err)
	}
	if _, err := r.ReadString('\n'); err != nil {
		c.Close()
		t.Fatal(err)
	}
	if r.Buffered() != 0 {
		c.Close()
		t.Fatal("unexpected early relay payload")
	}
	return c
}

func TestShortManagedRelayPreservesDirectionalEOFAndClosesEveryOwnedListener(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	payload := bytes.Repeat([]byte("short-https-payload"), 300)
	done := make(chan error, 1)
	go func() {
		peer, err := backend.Accept()
		if err != nil {
			done <- err
			return
		}
		defer peer.Close()
		peer.SetDeadline(time.Now().Add(15 * time.Second))
		got, err := io.ReadAll(peer)
		if err != nil {
			done <- err
			return
		}
		if !bytes.Equal(got, payload) {
			done <- fmt.Errorf("wrong upload: %d bytes", len(got))
			return
		}
		time.Sleep(20 * time.Millisecond)
		_, err = peer.Write(append([]byte("reply:"), got...))
		done <- err
	}()
	s, c := shortFixture(t, backend.Addr().String())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sh, err := StartShortServer(ctx, &s, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	ch, err := StartShortClient(ctx, &c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	conn := shortConnect(t, ch, c.RemoteAddr)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	conn.Close()
	if err != nil || !bytes.Equal(got, append([]byte("reply:"), payload...)) {
		t.Fatalf("delayed reply: %d bytes: %v", len(got), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cancel()
	ch.Close()
	sh.Close()
	l, err := net.Listen("tcp", s.Short.Listen)
	if err != nil {
		t.Fatal("public listener survived shutdown:", err)
	}
	l.Close()
}

func TestShortManagedRelayRejectsMixedEnginesAndAnUnusableACMERenewalConfig(t *testing.T) {
	s, c := shortFixture(t, "127.0.0.1:62101")
	for _, change := range []func(*config.Config){
		func(v *config.Config) { v.Client = c },
		func(v *config.Config) { v.Server.Sing.Protocol = "tuic" },
		func(v *config.Config) { v.Server.Short.ACMEDomain = "127.0.0.1" },
		func(v *config.Config) { v.Server.Short.Listen = v.Server.BindAddr },
		func(v *config.Config) { v.Server.MPTCP = true },
	} {
		v := config.Config{Server: s}
		change(&v)
		if ValidateShort(&v) == nil {
			t.Fatal("accepted an unusable managed relay")
		}
	}
	s.Short = config.ShortServerConfig{Listen: "0.0.0.0:443", ServerName: "relay.example.com", ACMEDomain: "relay.example.com"}
	if err := ValidateShort(&config.Config{Server: s}); err != nil {
		t.Fatal("valid ACME config rejected:", err)
	}
	for _, change := range []func(*config.ClientConfig){
		func(v *config.ClientConfig) { v.Token = "backpack" },
		func(v *config.ClientConfig) { v.ConnectionPool = 64 },
		func(v *config.ClientConfig) { v.AggressivePool = true },
	} {
		v := c
		change(&v)
		if ValidateShort(&config.Config{Client: v}) == nil {
			t.Fatal("unsafe Short client capacity or credentials accepted")
		}
	}
}
