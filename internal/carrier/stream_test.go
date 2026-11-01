package carrier

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func TestStreamCarriersCarryBinaryTraffic(t *testing.T) {
	for _, mode := range []config.TransportType{config.TLS, config.GRPC} {
		for _, browser := range []string{"chrome", "firefox", "safari", "edge", "random"} {
			t.Run(string(mode)+"/"+browser, func(t *testing.T) {
				cx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				server := &Config{Mode: mode, TLS: testTLS(t)}
				done := make(chan error, 1)
				go func() {
					raw, err := ln.Accept()
					if err != nil {
						done <- err
						return
					}
					defer raw.Close()
					c, err := server.Server(cx, raw)
					if err != nil {
						done <- err
						return
					}
					defer c.Close()
					_, err = io.Copy(c, c)
					done <- err
				}()
				raw, err := net.Dial("tcp", ln.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				client := &Config{Mode: mode, Options: config.CarrierOptions{ServerName: "localhost", Fingerprint: browser, TLSFragment: true}}
				c, err := client.Client(cx, raw, ln.Addr().String(), 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				payload := make([]byte, 256<<10)
				rand.Read(payload)
				sent := make(chan error, 1)
				go func() { _, e := writeFull(c, payload); sent <- e }()
				got := make([]byte, len(payload))
				if _, err := io.ReadFull(c, got); err != nil {
					t.Fatal(err)
				}
				if err := <-sent; err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(payload, got) {
					t.Fatal("binary payload changed")
				}
				c.Close()
				select {
				case <-done:
				case <-cx.Done():
					t.Fatal("carrier did not release its server")
				}
			})
		}
	}
}

func TestCarrierHandshakeCancelsPromptly(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	cx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := (&Config{Mode: config.TLS}).Client(cx, a, "localhost:443", 10*time.Second)
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("handshake did not cancel promptly: %v", err)
	}
}

func TestGRPCRejectsUnboundedAndCompressedFrames(t *testing.T) {
	for _, h := range [][]byte{{1, 0, 0, 0, 1}, {0, 0x7f, 0xff, 0xff, 0xff}} {
		c := &grpcConn{reader: bytes.NewReader(h)}
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("invalid frame was accepted")
		}
	}
}

func TestGRPCWriteAfterCloseNeverTouchesTheFinishedHandler(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	finished := make(chan struct{})
	writer := &finishedHandlerWriter{finished: finished}
	c := &grpcConn{Conn: raw, writer: writer, closeFn: func() { close(finished) }}
	c.Close()
	// A control goroutine can send its farewell after another goroutine closes
	// this carrier. http2.ResponseWriter panics if that reaches the old handler.
	for i := 0; i < 100; i++ {
		if n, err := c.Write([]byte("farewell")); n != 0 || !errors.Is(err, net.ErrClosed) {
			t.Fatalf("write after close returned %d, %v", n, err)
		}
	}
}

type finishedHandlerWriter struct{ finished <-chan struct{} }

func (w *finishedHandlerWriter) Write(p []byte) (int, error) {
	select {
	case <-w.finished:
		panic("Write called after Handler finished")
	default:
		return len(p), nil
	}
}

func TestGRPCIPv6LiteralCarriesBinaryTraffic(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("IPv6 loopback unavailable:", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server := &Config{Mode: config.GRPC, TLS: testTLS(t)}
	done := make(chan error, 1)
	go func() {
		raw, e := ln.Accept()
		if e != nil {
			done <- e
			return
		}
		defer raw.Close()
		c, e := server.Server(ctx, raw)
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		_, e = io.Copy(c, c)
		done <- e
	}()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp6", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	c, err := (&Config{Mode: config.GRPC}).Client(ctx, raw, ln.Addr().String(), time.Second*3)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second * 3))
	payload := []byte{0, 1, 127, 128, 255}
	if _, err = c.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("IPv6 gRPC payload changed")
	}
	c.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("IPv6 gRPC handler was not released")
	}
}
