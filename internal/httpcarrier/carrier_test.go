package httpcarrier

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

func tlsPair(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"carrier.backpack.invalid"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, &tls.Config{ServerName: "carrier.backpack.invalid", RootCAs: roots}
}

func echoTarget(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer c.Close()
				stop := context.AfterFunc(ctx, func() { c.Close() })
				defer stop()
				io.Copy(c, c)
			})
		}
	})
	t.Cleanup(func() { cancel(); l.Close(); workers.Wait() })
	return l.Addr().String()
}

func exchange(ctx context.Context, c *Client, target string, payload []byte) error {
	s, err := c.DialStream(ctx, target)
	if err != nil {
		return err
	}
	defer s.Close()
	done := make(chan error, 1)
	go func() {
		_, err := s.Write(payload)
		if err == nil {
			err = s.(interface{ CloseWrite() error }).CloseWrite()
		}
		done <- err
	}()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	got, err := io.ReadAll(s)
	if err != nil {
		return err
	}
	if err := <-done; err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func TestCarriersTransferConcurrentStreamsAndDirectionalEOF(t *testing.T) {
	for _, tc := range []struct{ protocol, mode, path string }{{"trusttunnel", "h2", ""}, {"trusttunnel", "h3", ""}, {"trusttunnel", "auto", ""}, {"webtransport", "h3", "/private-carrier"}} {
		t.Run(tc.protocol+"-"+tc.mode, func(t *testing.T) {
			serverTLS, clientTLS := tlsPair(t)
			target := echoTarget(t)
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			s, err := Listen(ctx, tc.protocol, tc.mode, "127.0.0.1:0", target, "backpack", "separate-secret-123456", tc.path, serverTLS)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			c, err := NewClient(ctx, tc.protocol, tc.mode, s.Addr().String(), "backpack", "separate-secret-123456", tc.path, clientTLS)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			payload := make([]byte, 192*1024)
			rand.Read(payload)
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					if err := exchange(ctx, c, target, payload); err != nil {
						t.Errorf("verified stream: %v", err)
					}
				})
			}
			wg.Wait()
			if err := exchange(ctx, c, target, []byte("fresh stream after directional EOF")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTrustTunnelFallsBackWhenUDPHasNoServer(t *testing.T) {
	st, ct := tlsPair(t)
	target := echoTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s, err := Listen(ctx, "trusttunnel", "h2", "127.0.0.1:0", target, "backpack", "separate-secret-123456", "", st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := NewClient(ctx, "trusttunnel", "auto", s.Addr().String(), "backpack", "separate-secret-123456", "", ct)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := exchange(ctx, c, target, []byte("HTTP/2 fallback carries verified data")); err != nil {
		t.Fatal(err)
	}
}

func TestCarriersRejectAuthenticationAndTrustFailures(t *testing.T) {
	for _, protocol := range []string{"trusttunnel", "webtransport"} {
		t.Run(protocol, func(t *testing.T) {
			st, ct := tlsPair(t)
			target := echoTarget(t)
			mode, path := "h2", ""
			if protocol == "webtransport" {
				mode, path = "h3", "/private-carrier"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			s, err := Listen(ctx, protocol, mode, "127.0.0.1:0", target, "backpack", "separate-secret-123456", path, st)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for _, bad := range []string{"password", "identity", "path"} {
				if protocol == "trusttunnel" && bad == "path" {
					continue
				}
				t.Run(bad, func(t *testing.T) {
					tlsCfg := ct.Clone()
					password, p := "separate-secret-123456", path
					if bad == "password" {
						password = "wrong-password-123456"
					}
					if bad == "identity" {
						tlsCfg.ServerName = "wrong.backpack.invalid"
					}
					if bad == "path" {
						p = "/wrong-path"
					}
					c, err := NewClient(ctx, protocol, mode, s.Addr().String(), "backpack", password, p, tlsCfg)
					if err != nil {
						t.Fatal(err)
					}
					defer c.Close()
					stream, err := c.DialStream(ctx, target)
					if stream != nil {
						stream.Close()
					}
					if err == nil {
						t.Fatal("invalid identity or credentials accepted")
					}
				})
			}
		})
	}
}

func TestTrustTunnelRefusesAnotherDestination(t *testing.T) {
	st, ct := tlsPair(t)
	target := echoTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Listen(ctx, "trusttunnel", "h2", "127.0.0.1:0", target, "backpack", "separate-secret-123456", "", st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := NewClient(ctx, "trusttunnel", "h2", s.Addr().String(), "backpack", "separate-secret-123456", "", ct)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := (&http.Request{Method: http.MethodConnect, URL: &url.URL{Scheme: "https", Host: s.Addr().String()}, Host: "127.0.0.1:1", Header: http.Header{"Proxy-Authorization": []string{authorization("backpack", "separate-secret-123456")}}}).WithContext(ctx)
	resp, err := c.h2.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong target status: %d", resp.StatusCode)
	}
}
