package naive

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

func singFixture(t *testing.T, protocol string) (config.ServerConfig, config.ClientConfig) {
	t.Helper()
	c, cert, key := xhttpFixture(t)
	x := config.SingServerConfig{Binary: c.Xray.Binary, Protocol: protocol, Listen: "127.0.0.1:62102", ServerName: c.Xray.ServerName, Password: "a-distinct-helper-password", Certificate: cert, Key: key}
	y := config.SingClientConfig{Binary: x.Binary, Protocol: protocol, Server: x.Listen, ServerName: x.ServerName, Password: x.Password, CAFile: cert}
	if protocol == "tuic" {
		x.UUID = c.Xray.UUID
		y.UUID = x.UUID
	}
	if protocol == "hysteria2" {
		x.ObfsPassword = "a-distinct-salamander-password"
		y.ObfsPassword = x.ObfsPassword
	}
	if protocol == "shadowtls" {
		x.Certificate, x.Key = "", ""
		x.Handshake = "127.0.0.1:62103"
		x.ShadowsocksKey = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
		y.ShadowsocksKey = x.ShadowsocksKey
	}
	return config.ServerConfig{Transport: config.TCP, BindAddr: c.RemoteAddr, Sing: x}, config.ClientConfig{Transport: config.TCP, RemoteAddr: c.RemoteAddr, Sing: y}
}

func TestSingRejectsMismatchedRolesAndUnusableCredentials(t *testing.T) {
	for _, protocol := range []string{"hysteria2", "anytls", "shadowtls", "tuic"} {
		t.Run(protocol, func(t *testing.T) {
			s, c := singFixture(t, protocol)
			if err := ValidateSing(&config.Config{Server: s}); err != nil {
				t.Fatal(err)
			}
			if err := ValidateSing(&config.Config{Client: c}); err != nil {
				t.Fatal(err)
			}
			for name, change := range map[string]func(*config.Config){
				"both roles":       func(v *config.Config) { v.Client = c },
				"external target":  func(v *config.Config) { v.Server.BindAddr = "203.0.113.1:1234" },
				"other helper":     func(v *config.Config) { v.Server.Naive.Binary = s.Sing.Binary },
				"weak password":    func(v *config.Config) { v.Server.Sing.Password = "short" },
				"unknown protocol": func(v *config.Config) { v.Server.Sing.Protocol = "fallback" },
				"wrong fields":     func(v *config.Config) { v.Server.Sing.UUID = "not-a-uuid" },
			} {
				t.Run(name, func(t *testing.T) {
					v := config.Config{Server: s}
					change(&v)
					if ValidateSing(&v) == nil {
						t.Fatal("accepted invalid carrier")
					}
				})
			}
			if _, err := SingClientJSON(c.Sing, "0.0.0.0:1080", c.RemoteAddr, "tcp"); err == nil {
				t.Fatal("public SOCKS listener accepted")
			}
			if _, err := SingClientJSON(c.Sing, "127.0.0.1:1080", "example.com:443", "tcp"); err == nil {
				t.Fatal("arbitrary destination accepted")
			}
		})
	}
}

func TestSingShadowTLSRequiresACoverIdentitySeparateFromItsReverseTarget(t *testing.T) {
	s, c := singFixture(t, "shadowtls")
	c.Sing.ServerName = "127.0.0.1"
	if err := ValidateSingClient(&c); err == nil {
		t.Fatal("accepted a literal IP as the verified cover identity")
	}
	s.Sing.Handshake = s.BindAddr
	if err := ValidateSingServer(&s); err == nil {
		t.Fatal("accepted a cover that loops into the private reverse target")
	}
}

func singFreeAddr(t *testing.T, udp bool) string {
	t.Helper()
	if udp {
		l, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		a := l.LocalAddr().String()
		l.Close()
		return a
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}

// This test runs the actual pinned helper contract, including a local real TLS
// 1.3 cover for ShadowTLS. No Internet endpoint is necessary or trusted by the test.
func TestSingCarriersTransferAuthenticatedBytesAndRejectOtherTargets(t *testing.T) {
	bin := os.Getenv("BP_SINGBOX_BINARY")
	if bin == "" {
		t.Skip("set BP_SINGBOX_BINARY to the pinned sing-box executable")
	}
	for _, protocol := range []string{"hysteria2", "anytls", "shadowtls", "tuic"} {
		t.Run(protocol, func(t *testing.T) {
			s, c := singFixture(t, protocol)
			s.Sing.Binary, c.Sing.Binary = bin, bin
			s.Sing.Listen = singFreeAddr(t, config.SingDatagram(protocol))
			c.Sing.Server = s.Sing.Listen
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			log := logrus.New()
			log.SetOutput(io.Discard)
			out, err := os.Create(filepath.Join(t.TempDir(), "helper.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			ctx = WithHelperOutput(ctx, out)
			t.Cleanup(func() {
				if t.Failed() {
					if b, err := os.ReadFile(out.Name()); err == nil {
						t.Logf("helper diagnostics:\n%s", b)
					}
				}
			})
			if protocol == "shadowtls" {
				// Use the original matching key rather than generating a second identity.
				key := filepath.Join(filepath.Dir(c.Sing.CAFile), "key.pem")
				pair, err := tls.LoadX509KeyPair(c.Sing.CAFile, key)
				if err != nil {
					t.Fatal(err)
				}
				cover, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13})
				if err != nil {
					t.Fatal(err)
				}
				defer cover.Close()
				s.Sing.Handshake = cover.Addr().String()
				go func() {
					for {
						v, err := cover.Accept()
						if err != nil {
							return
						}
						go func() {
							defer v.Close()
							v.SetDeadline(time.Now().Add(5 * time.Second))
							_ = v.(*tls.Conn).Handshake()
							_, _ = io.Copy(io.Discard, v)
						}()
					}
				}()
			}
			backend, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			s.BindAddr, c.RemoteAddr = backend.Addr().String(), backend.Addr().String()
			go func() {
				for {
					v, err := backend.Accept()
					if err != nil {
						return
					}
					go func() { defer v.Close(); _, _ = io.Copy(v, v) }()
				}
			}()
			sh, err := StartSingServer(ctx, &s, log)
			if err != nil {
				t.Fatal(err)
			}
			defer sh.Close()
			ch, err := StartSingClient(ctx, &c, log)
			if err != nil {
				t.Fatal(err)
			}
			defer ch.Close()
			d, err := proxy.SOCKS5("tcp", ch.addr, nil, proxy.Direct)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := d.(proxy.ContextDialer).DialContext(ctx, "tcp", c.RemoteAddr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			payload := bytes.Repeat([]byte("verified tunnel payload\x00"), 16384)
			written := make(chan error, 1)
			go func() { _, err := conn.Write(payload); written <- err }()
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatal(err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload, got) {
				t.Fatal("corrupt bidirectional payload")
			}
			other, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			attempt, stop := context.WithTimeout(ctx, 2*time.Second)
			bad, err := d.(proxy.ContextDialer).DialContext(attempt, "tcp", other.Addr().String())
			stop()
			if err == nil {
				bad.Close()
				t.Fatal("helper escaped its fixed target")
			}
			for _, fault := range []string{"password", "trust"} {
				badCfg := c
				if fault == "password" {
					badCfg.Sing.Password = "a-different-invalid-helper-password"
				} else {
					badCfg.Sing.CAFile = ""
				}
				badHelper, err := StartSingClient(ctx, &badCfg, log)
				if err != nil {
					t.Fatal(err)
				}
				badDialer, err := proxy.SOCKS5("tcp", badHelper.addr, nil, proxy.Direct)
				if err != nil {
					badHelper.Close()
					t.Fatal(err)
				}
				attempt, stop := context.WithTimeout(ctx, 2*time.Second)
				v, err := badDialer.(proxy.ContextDialer).DialContext(attempt, "tcp", c.RemoteAddr)
				if err == nil {
					v.SetDeadline(time.Now().Add(time.Second))
					_, err = v.Write([]byte{42})
					if err == nil {
						var reply [1]byte
						_, err = io.ReadFull(v, reply[:])
					}
					v.Close()
				}
				stop()
				badHelper.Close()
				if err == nil {
					t.Fatalf("%s mismatch still carried authenticated bytes", fault)
				}
			}
			// Native UDP uses the same protocol without a reliable stream wrapper.
			if config.SingDatagram(protocol) {
				udp, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer udp.Close()
				go func() {
					var b [2048]byte
					for {
						n, a, e := udp.ReadFrom(b[:])
						if e != nil {
							return
						}
						_, _ = udp.WriteTo(b[:n], a)
					}
				}()
				ch.Close()
				sh.Close()
				sh, err = StartSingListener(ctx, s.Sing, udp.LocalAddr().String(), "udp", log)
				if err != nil {
					t.Fatal(err)
				}
				defer sh.Close()
				ch, err = StartSingProxy(ctx, c.Sing, udp.LocalAddr().String(), "udp", log)
				if err != nil {
					t.Fatal(err)
				}
				defer ch.Close()
				pc, err := NewSOCKSPacketConn(ctx, ch.addr, udp.LocalAddr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer pc.Close()
				for i := 0; i < 20; i++ {
					b := bytes.Repeat([]byte{byte(i)}, 1000)
					pc.SetDeadline(time.Now().Add(3 * time.Second))
					if _, err := pc.WriteTo(b, udp.LocalAddr()); err != nil {
						t.Fatal(err)
					}
					var p [1200]byte
					n, _, err := pc.ReadFrom(p[:])
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(b, p[:n]) {
						t.Fatal("datagram corruption or boundary loss")
					}
				}
				// Recreate the helper on the same reserved SOCKS endpoint. The
				// existing PacketConn must notice the lost TCP association and
				// reconnect without replacing the L3 carrier or buffering traffic.
				body, err := os.ReadFile(filepath.Join(ch.dir, "helper.json"))
				if err != nil {
					t.Fatal(err)
				}
				addr := ch.addr
				ch.Close()
				replacement, err := startManaged(ctx, bin, "sing-client", body, addr, os.Environ(), log, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer replacement.Close()
				recovered := false
				for until := time.Now().Add(12 * time.Second); time.Now().Before(until); {
					pc.SetDeadline(time.Now().Add(500 * time.Millisecond))
					payload := []byte("after helper restart")
					_, err = pc.WriteTo(payload, udp.LocalAddr())
					if err != nil {
						t.Fatal(err)
					}
					var got [64]byte
					n, _, err := pc.ReadFrom(got[:])
					if err == nil && bytes.Equal(got[:n], payload) {
						recovered = true
						break
					}
				}
				if !recovered {
					t.Fatal("native datagram association did not recover after helper restart")
				}
			}
		})
	}
}
