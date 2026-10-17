package e2e

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

func TestSingReverseEnginesCarryConcurrentTCPUDPAndDirectionalEOF(t *testing.T) {
	bin := os.Getenv("BP_SINGBOX_BINARY")
	if bin == "" {
		t.Skip("set BP_SINGBOX_BINARY to test actual managed reverse engines")
	}
	cover := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("cover")) }))
	defer cover.Close()
	material := cover.TLS.Certificates[0]
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: material.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKCS8PrivateKey(material.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	name := cover.Certificate().DNSNames[0]
	for _, variant := range []string{"hysteria2", "anytls", "shadowtls", "tuic", "hysteria2-native", "tuic-native"} {
		t.Run(variant, func(t *testing.T) {
			protocol := strings.TrimSuffix(variant, "-native")
			backend := startEchoBackend(t)
			udpBackend := startUDPEcho(t)
			eof, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer eof.Close()
			go func() {
				for {
					v, err := eof.Accept()
					if err != nil {
						return
					}
					go func() {
						defer v.Close()
						body, err := io.ReadAll(v)
						if err == nil {
							v.Write(append([]byte("delayed reply:"), body...))
						}
					}()
				}
			}()
			internal, entry, public, udpEntry, eofEntry := freePort(t), freePort(t), freePort(t), freePort(t), freePort(t)
			s := baseServerConfig("tcp", internal, entry, backend.addr, "sing-reverse-token-0123456789abcdef")
			c := baseClientConfig("tcp", s.BindAddr, s.Token, nil)
			s.Ports = append(s.Ports, fmt.Sprintf("127.0.0.1:%d=%s", udpEntry, udpBackend), fmt.Sprintf("127.0.0.1:%d=%s", eofEntry, eof.Addr()))
			on := true
			s.AcceptUDP = &on
			s.Sing = config.SingServerConfig{Binary: bin, Protocol: protocol, Listen: fmt.Sprintf("127.0.0.1:%d", public), ServerName: name, Password: "separate-helper-account-secret", Certificate: cert, Key: key}
			c.Sing = config.SingClientConfig{Binary: bin, Protocol: protocol, Server: s.Sing.Listen, ServerName: name, Password: s.Sing.Password, CAFile: cert}
			s.Sing.NativeUDP, c.Sing.NativeUDP = strings.HasSuffix(variant, "-native"), strings.HasSuffix(variant, "-native")
			if protocol == "tuic" {
				s.Sing.UUID = "12345678-1234-4234-8234-123456789abc"
				c.Sing.UUID = s.Sing.UUID
			}
			if protocol == "hysteria2" {
				s.Sing.ObfsPassword = "separate-salamander-obfuscation-secret"
				c.Sing.ObfsPassword = s.Sing.ObfsPassword
			}
			if protocol == "shadowtls" {
				s.Sing.Certificate, s.Sing.Key = "", ""
				s.Sing.Handshake = cover.Listener.Addr().String()
				s.Sing.ShadowsocksKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{17}, 32))
				c.Sing.ShadowsocksKey = s.Sing.ShadowsocksKey
			}
			tun := runPair(t, s, c, entry, internal)
			if err := tun.waitReady(25 * time.Second); err != nil {
				t.Fatal(err)
			}
			payload := randomPayload(t, 1<<20)
			results := make(chan error, 4)
			for i := 0; i < 4; i++ {
				go func() { results <- tun.roundTrip(payload) }()
			}
			for i := 0; i < 4; i++ {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			v, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", eofEntry), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			v.SetDeadline(time.Now().Add(5 * time.Second))
			request := []byte("reply after EOF")
			if _, err := v.Write(request); err != nil {
				t.Fatal(err)
			}
			if err := v.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			want := append([]byte("delayed reply:"), request...)
			got := make([]byte, len(want))
			if _, err := io.ReadFull(v, got); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("reply after directional EOF: %v", err)
			}
			u, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", udpEntry), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer u.Close()
			u.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := u.Write(request); err != nil {
				t.Fatal(err)
			}
			var b [100]byte
			n, err := u.Read(b[:])
			if err != nil || !bytes.Equal(b[:n], request) {
				t.Fatalf("forwarded UDP: %v", err)
			}
			tun.Stop()
			waitPortFree(t, public, 3*time.Second)
		})
	}
}
