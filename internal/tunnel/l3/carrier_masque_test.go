package l3

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/tls"
	"github.com/backpack/backpack/config"
	"github.com/quic-go/quic-go/http3"
)

func masqueTestConfig(t *testing.T) config.MasqueConfig {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := udp.LocalAddr().String()
	udp.Close()
	return config.MasqueConfig{Target: target, CertFile: certFile, KeyFile: keyFile, CAFile: certFile, ServerName: "localhost"}
}

func masqueTestListener(t *testing.T) (DatagramCarrier, Config) {
	t.Helper()
	cfg := Config{Mode: ModeListen, Addr: "127.0.0.1:0", Token: "masque-test-long-secret", Carrier: CarrierMasque,
		Masque: masqueTestConfig(t)}
	c, _, err := openMasque(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	cfg.Mode, cfg.Addr = ModeDial, c.LocalAddr().String()
	return c, cfg
}

func TestMasqueDatagramsAndReconnect(t *testing.T) {
	listener, cfg := masqueTestListener(t)
	for attempt := 0; attempt < 3; attempt++ {
		dialer, peer, err := openMasque(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte{byte(attempt + 1)}, 1100)
		if _, err := dialer.WriteTo(payload, peer); err != nil {
			t.Fatal(err)
		}
		listener.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 2048)
		n, from, err := listener.ReadFrom(buf)
		if err != nil || !bytes.Equal(buf[:n], payload) {
			t.Fatalf("up: %d bytes, %v", n, err)
		}
		if _, err := listener.WriteTo(buf[:n], from); err != nil {
			t.Fatal(err)
		}
		dialer.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, _, err = dialer.ReadFrom(buf)
		if err != nil || !bytes.Equal(buf[:n], payload) {
			t.Fatalf("down: %d bytes, %v", n, err)
		}
		dialer.Close()
	}
}

func TestMasqueRejectsBadCredentialsTargetsAndCertificates(t *testing.T) {
	_, base := masqueTestListener(t)
	for name, change := range map[string]func(*Config){
		"token":                 func(c *Config) { c.Token = "wrong" },
		"target":                func(c *Config) { c.Masque.Target = "127.0.0.1:1" },
		"hostname":              func(c *Config) { c.Masque.ServerName = "wrong.example" },
		"untrusted certificate": func(c *Config) { c.Masque.CAFile = "" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			change(&cfg)
			c, _, err := openMasque(context.Background(), cfg)
			if err == nil {
				c.Close()
				t.Fatal("accepted invalid settings")
			}
		})
	}
}

func TestMasqueReadDeadlineCanChangeAndReset(t *testing.T) {
	_, cfg := masqueTestListener(t)
	c, _, err := openMasque(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	result := make(chan error, 1)
	go func() { _, _, err := c.ReadFrom(make([]byte, 10)); result <- err }()
	c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	select {
	case err := <-result:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("setting deadline did not wake an existing read")
	}
	c.SetReadDeadline(time.Time{})
	go func() { _, _, err := c.ReadFrom(make([]byte, 10)); result <- err }()
	c.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed read succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not release read")
	}
}

func TestMasqueCannotProxyAnExternalTarget(t *testing.T) {
	for _, target := range []string{"8.8.8.8:53", "example.com:53", "0.0.0.0:9", "127.0.0.1:0", "127.0.0.1:65536"} {
		cfg := Config{Mode: ModeDial, Masque: config.MasqueConfig{Target: target}}
		if _, _, err := openMasque(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("target %q: %v", target, err)
		}
	}
}

func TestMasqueIgnoresPacketsOutsideAuthenticatedProxySockets(t *testing.T) {
	listener, cfg := masqueTestListener(t)
	sock, err := net.Dial("udp", cfg.Masque.TargetAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	if _, err := sock.Write([]byte("unregistered local injection")); err != nil {
		t.Fatal(err)
	}
	listener.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := listener.ReadFrom(make([]byte, 128)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("injected datagram reached tunnel: %v", err)
	}
}

func TestMasqueSetupCancellationWhileWaitingForResponse(t *testing.T) {
	m := masqueTestConfig(t)
	pair, err := tls.LoadX509KeyPair(m.CertFile, m.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	requested := make(chan struct{}, 1)
	server := &http3.Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}}, EnableDatagrams: true,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case requested <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		})}
	defer server.Close()
	go server.Serve(udp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		c, _, err := openMasque(ctx, Config{Mode: ModeDial, Addr: udp.LocalAddr().String(), Token: "token", Masque: m})
		if c != nil {
			c.Close()
		}
		finished <- err
	}()
	select {
	case <-requested:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not reach server")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled setup succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled setup is stuck reading HTTP response")
	}
}

func TestMasqueTunnelCarriesIPOverNoise(t *testing.T) {
	masqueCfg := masqueTestConfig(t)
	dialDev, listenDev := newFakeDevice(1400), newFakeDevice(1400)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	base := Config{Mode: ModeListen, Addr: "127.0.0.1:0", Token: "masque-noise-token", Carrier: CarrierMasque,
		Masque: masqueCfg, LocalIP: "10.10.0.2/30", PeerIP: "10.10.0.1", MTU: 1300}
	listener, err := New(base, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	listener.openDevice = func(deviceSpec) (packetDevice, error) { return listenDev, nil }
	start(t, ctx, cancel, listener, listenDev)
	base.Mode, base.Addr = ModeDial, awaitBind(t, listener).String()
	base.LocalIP, base.PeerIP = "10.10.0.1/30", "10.10.0.2"
	dialer, err := New(base, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	dialer.openDevice = func(deviceSpec) (packetDevice, error) { return dialDev, nil }
	start(t, ctx, cancel, dialer, dialDev)
	awaitSession(t, dialer, 10*time.Second)
	across(t, dialDev, listenDev, ipv4Packet(bytes.Repeat([]byte{0xAB}, 1100)...))
	across(t, listenDev, dialDev, ipv4Packet(4, 3, 2, 1))
}
