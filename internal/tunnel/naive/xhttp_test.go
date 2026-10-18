package naive

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

func xhttpFixture(t *testing.T, extraNames ...string) (config.ClientConfig, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(42), DNSNames: []string{"origin.backpack.invalid", "upload.backpack.invalid", "download.backpack.invalid"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	leaf.DNSNames = append(leaf.DNSNames, extraNames...)
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cert, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return config.ClientConfig{Transport: config.TCP, RemoteAddr: "127.0.0.1:62101", Xray: config.XrayClientConfig{Binary: binary, Mode: "xhttp", Server: "127.0.0.1:62102", UUID: "12345678-1234-4234-8234-123456789abc", ServerName: "upload.backpack.invalid", Path: "/private-xhttp", CAFile: cert}}, cert, kp
}

func TestXHTTPOptionsAreValidatedBeforeStartingAHelper(t *testing.T) {
	c, _, _ := xhttpFixture(t)
	for name, change := range map[string]func(*config.XrayClientConfig){
		"negative":           func(x *config.XrayClientConfig) { x.MaxRequests = -1 },
		"unbounded":          func(x *config.XrayClientConfig) { x.MaxConnections = 65 },
		"conflicting limits": func(x *config.XrayClientConfig) { x.MaxConnections = 2; x.MaxConcurrency = 2 },
		"missing endpoint":   func(x *config.XrayClientConfig) { x.DownloadHost = "download.backpack.invalid" },
		"invalid endpoint": func(x *config.XrayClientConfig) {
			x.DownloadServer = "https://example.com:443"
			x.DownloadServerName = "example.com"
		},
		"missing identity": func(x *config.XrayClientConfig) { x.DownloadServer = "127.0.0.1:443" },
		"path injection": func(x *config.XrayClientConfig) {
			x.DownloadServer = "127.0.0.1:443"
			x.DownloadServerName = "example.com"
			x.DownloadPath = "/bad?query"
		},
		"missing trust": func(x *config.XrayClientConfig) {
			x.DownloadServer = "127.0.0.1:443"
			x.DownloadServerName = "example.com"
			x.DownloadCAFile = filepath.Join(t.TempDir(), "missing")
		},
		"reality tuning": func(x *config.XrayClientConfig) { x.Mode = "reality"; x.MaxRequests = 3 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := c
			change(&bad.Xray)
			if ValidateXrayClient(&bad) == nil {
				t.Fatal("accepted invalid XHTTP options")
			}
		})
	}
}

func TestXHTTPDownloadIdentityAndMuxDefaults(t *testing.T) {
	c, cert, _ := xhttpFixture(t)
	c.Xray.MaxConnections = 2
	c.Xray.MaxRequests = 400
	c.Xray.DownloadServer = "127.0.0.1:62103"
	c.Xray.DownloadServerName = "download.backpack.invalid"
	c.Xray.DownloadCAFile = cert
	body, err := XrayClientJSON(&c, "127.0.0.1:62104")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	stream := root["outbounds"].([]any)[1].(map[string]any)["streamSettings"].(map[string]any)
	x := stream["xhttpSettings"].(map[string]any)
	down := x["downloadSettings"].(map[string]any)
	mux := x["xmux"].(map[string]any)
	if x["mode"] != "stream-up" || mux["maxConnections"] != float64(2) || mux["maxConcurrency"] != nil || mux["hMaxReusableSecs"] != "1800-3000" {
		t.Fatal("unsafe mux defaults")
	}
	tls := down["tlsSettings"].(map[string]any)
	if tls["serverName"] != "download.backpack.invalid" || tls["allowInsecure"] != false || down["port"] != float64(62103) || down["xhttpSettings"].(map[string]any)["downloadSettings"] != nil {
		t.Fatal("incorrect independent download identity")
	}
	c.Xray.DownloadCAFile = ""
	body, err = XrayClientJSON(&c, "127.0.0.1:62104")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(body, []byte(`"certificates"`)) != 1 {
		t.Fatal("download silently inherited upload private CA")
	}
	var copied config.XrayClientConfig
	copied.Mode = "xhttp"
	copied.CopyXHTTPOptions(c.Xray)
	if copied.DownloadServer != c.Xray.DownloadServer || copied.MaxConnections != 2 {
		t.Fatal("lost local settings")
	}
	copied = config.XrayClientConfig{Mode: "reality"}
	copied.CopyXHTTPOptions(c.Xray)
	if copied.DownloadServer != "" {
		t.Fatal("XHTTP settings leaked into REALITY")
	}
}

// Run with the same verified pinned Xray binary used by installation and CI.
// Two independent TLS/H2 frontends prove the actual helper uses both routes.
func TestXHTTPSplitDownloadWithPinnedHelper(t *testing.T) {
	binary := os.Getenv("BP_XRAY_BINARY")
	if binary == "" {
		t.Skip("set BP_XRAY_BINARY to the verified pinned Xray helper")
	}
	c, cert, key := xhttpFixture(t)
	c.Xray.Binary = binary
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := backend.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		io.Copy(conn, conn)
	}()
	c.RemoteAddr = backend.Addr().String()
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := reserve.Addr().String()
	reserve.Close()
	s := config.ServerConfig{Transport: config.TCP, BindAddr: c.RemoteAddr, Xray: config.XrayServerConfig{Binary: binary, Mode: "xhttp", Listen: origin, UUID: c.Xray.UUID, ServerName: "origin.backpack.invalid", Path: c.Xray.Path, Certificate: cert, Key: key}}
	log := logrus.New()
	log.SetOutput(io.Discard)
	server, err := StartXrayServer(ctx, &s, log)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	pemBytes, _ := os.ReadFile(cert)
	roots.AppendCertsFromPEM(pemBytes)
	var uploads, downloads atomic.Int32
	front := func(counter *atomic.Int32) *httptest.Server {
		u, _ := url.Parse("https://" + origin)
		rp := httputil.NewSingleHostReverseProxy(u)
		transport := &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "origin.backpack.invalid"}}
		t.Cleanup(transport.CloseIdleConnections)
		rp.Transport = transport
		rp.FlushInterval = -1
		h := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			r.Host = "origin.backpack.invalid"
			rp.ServeHTTP(w, r)
		}))
		h.EnableHTTP2 = true
		h.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}
		h.StartTLS()
		return h
	}
	up := front(&uploads)
	defer up.Close()
	down := front(&downloads)
	defer down.Close()
	c.Xray.Server = up.Listener.Addr().String()
	c.Xray.DownloadServer = down.Listener.Addr().String()
	c.Xray.DownloadServerName = "download.backpack.invalid"
	c.Xray.DownloadCAFile = cert
	c.Xray.MaxConnections = 2
	c.Xray.MaxRequests = 400
	c.Xray.MaxReuseSeconds = 120
	client, err := StartXrayClient(ctx, &c, log)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	u, _ := url.Parse(client.ProxyURL())
	dialer, err := proxy.FromURL(u, &net.Dialer{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := dialer.Dial("tcp", c.RemoteAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(10 * time.Second))
	want := bytes.Repeat([]byte("verified split HTTP2 data\n"), 4096)
	writeDone := make(chan error, 1)
	go func() { _, err := stream.Write(want); writeDone <- err }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(stream, got); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) || uploads.Load() == 0 || downloads.Load() == 0 {
		t.Fatal("corrupted payload or separate download endpoint unused")
	}
	stream.Close()
	backend.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("backend did not close")
	}
}
