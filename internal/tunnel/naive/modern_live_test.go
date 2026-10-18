package naive

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

func modernBackend(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); c.SetDeadline(time.Now().Add(25 * time.Second)); io.Copy(c, c) }()
		}
	}()
	return l
}

func modernPayload(t *testing.T, ctx context.Context, h *Helper, target string) {
	t.Helper()
	d, err := proxy.SOCKS5("tcp", h.addr, nil, &net.Dialer{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.(proxy.ContextDialer).DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	payload := make([]byte, 96*1024)
	rand.Read(payload)
	done := make(chan error, 1)
	go func() { _, err := c.Write(payload); done <- err }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload corrupted")
	}
}

func TestXHTTPRealityOfficialPayloadAndConfig(t *testing.T) {
	binary := os.Getenv("BP_XRAY_BINARY")
	if binary == "" {
		t.Skip("set BP_XRAY_BINARY to pinned official Xray")
	}
	c, cert, key := xhttpFixture(t)
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	cover := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("cover")) }))
	cover.EnableHTTP2 = true
	cover.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}
	cover.StartTLS()
	defer cover.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	backend := modernBackend(t)
	c.RemoteAddr = backend.Addr().String()
	c.Xray.Binary, c.Xray.Mode, c.Xray.CAFile = binary, "xhttp-reality", ""
	c.Xray.Server = singFreeAddr(t, false)
	c.Xray.PublicKey = base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes())
	c.Xray.ShortID = "0123456789abcdef"
	s := config.ServerConfig{Transport: config.TCP, BindAddr: c.RemoteAddr, Xray: config.XrayServerConfig{Binary: binary, Listen: c.Xray.Server, Mode: c.Xray.Mode, UUID: c.Xray.UUID, ServerName: c.Xray.ServerName, Path: c.Xray.Path, PrivateKey: base64.RawURLEncoding.EncodeToString(private.Bytes()), ShortID: c.Xray.ShortID, Target: cover.Listener.Addr().String()}}
	log := logrus.New()
	log.SetOutput(io.Discard)
	server, err := StartXrayServer(ctx, &s, log)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := StartXrayClient(ctx, &c, log)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	modernPayload(t, ctx, client, c.RemoteAddr)
}

func TestTailcatOfficialLocalDERPAndAuthenticatedPayload(t *testing.T) {
	binary := os.Getenv("BP_TAILCAT_BINARY")
	if binary == "" {
		t.Skip("set BP_TAILCAT_BINARY to the separately pinned official sing-box")
	}
	base, cert, key := xhttpFixture(t, "localhost")
	t.Setenv("SSL_CERT_FILE", cert)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	serverKey, _ := NewTailcatKey()
	clientKey, _ := NewTailcatKey()
	psk, _ := NewTailcatKey()
	public, disco, err := TailcatPublic(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	derp := singFreeAddr(t, false)
	_, port, _ := net.SplitHostPort(derp)
	portNumber, _ := strconv.Atoi(port)
	stun := realmTestSTUN(t)
	_, stunPort, _ := net.SplitHostPort(stun)
	stunNumber, _ := strconv.Atoi(stunPort)
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(leaf.Raw)
	mapServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Regions": map[string]any{"901": map[string]any{"RegionID": 901, "RegionCode": "local", "RegionName": "isolated test DERP", "Nodes": []any{map[string]any{"Name": "local", "RegionID": 901, "HostName": "localhost", "IPv4": "127.0.0.1", "IPv6": "none", "DERPPort": portNumber, "STUNPort": stunNumber, "CertName": "sha256-raw:" + hex.EncodeToString(hash[:])}}}}})
	}))
	mapServer.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	mapServer.StartTLS()
	defer mapServer.Close()
	mapURL := "https://localhost:" + addrPortOnly(mapServer.Listener.Addr().String()) + "/derpmap.json"
	backend := modernBackend(t)
	s := config.ServerConfig{Transport: config.TCP, BindAddr: backend.Addr().String(), Sing: config.SingServerConfig{Protocol: "tailcat", Binary: binary, Listen: "127.0.0.1:62102", Tailcat: config.TailcatConfig{PrivateKey: serverKey, ClientKey: clientKey, PreSharedKey: psk, DERPMapURL: mapURL}}}
	c := config.ClientConfig{Transport: config.TCP, RemoteAddr: s.BindAddr, Sing: config.SingClientConfig{Protocol: "tailcat", Binary: binary, Server: s.Sing.Listen, Tailcat: config.TailcatConfig{PrivateKey: clientKey, PublicKey: public, DiscoKey: disco, PreSharedKey: psk, DERPMapURL: mapURL}}}
	ready := singFreeAddr(t, false)
	body, err := SingServerJSON(s.Sing, ready, s.BindAddr, "tcp")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	doc["services"] = []any{map[string]any{"type": "derp", "tag": "local-derp", "listen": "127.0.0.1", "listen_port": portNumber, "tls": map[string]any{"enabled": true, "certificate_path": cert, "key_path": key}, "config_path": filepath.Join(t.TempDir(), "derp.key"), "verify_client_inbound": []string{"public"}}}
	body, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	log := logrus.New()
	log.SetOutput(io.Discard)
	server, err := startManaged(ctx, binary, "sing-server", body, ready, os.Environ(), log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := StartSingClient(ctx, &c, log)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	modernPayload(t, ctx, client, c.RemoteAddr)
	// The helper's own generator independently verifies both key derivations.
	out, err := exec.CommandContext(ctx, binary, "generate", "tailcat-keypair").Output()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		parts := bytes.SplitN(line, []byte{':'}, 2)
		if len(parts) == 2 {
			values[string(parts[0])] = string(bytes.TrimSpace(parts[1]))
		}
	}
	p, d, err := TailcatPublic(values["PrivateKey"])
	if err != nil || p != values["PublicKey"] || d != values["DiscoKey"] {
		t.Fatal("Tailcat key derivation differs from official helper")
	}
	_ = base
}
