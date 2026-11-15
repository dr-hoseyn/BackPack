package direct

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDirectWebSocketCancellationInterruptsUpgrade(t *testing.T) {
	for _, transport := range []string{TransportWS, TransportWSS} {
		t.Run(transport, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if transport == TransportWSS {
				tlsConfig, err := originTLSConfig(&Config{}, quietLogger())
				if err != nil {
					t.Fatal(err)
				}
				listener = tls.NewListener(listener, tlsConfig)
			}
			reached := make(chan struct{})
			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(conn)
				if _, err := http.ReadRequest(reader); err != nil {
					serverDone <- err
					return
				}
				close(reached)
				// Accept the TCP/TLS connection and request, then withhold 101.
				_, err = reader.ReadByte()
				serverDone <- err
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				conn, err := dialWebSocket(ctx, &Config{Transport: transport, Addr: listener.Addr().String(), DialTimeout: time.Second})
				if conn != nil {
					conn.Close()
				}
				done <- err
			}()
			select {
			case <-reached:
			case err := <-serverDone:
				t.Fatalf("peer failed before upgrade: %v", err)
			case <-time.After(2 * time.Second):
				t.Fatal("upgrade request never reached peer")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled upgrade kept its socket and worker blocked")
			}
			select {
			case err := <-serverDone:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("peer did not observe socket close: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled upgrade left peer connection open")
			}
		})
	}
}

func TestDirectWebSocketDialReleasesCancellationOwnership(t *testing.T) {
	for _, transport := range []string{TransportWS, TransportWSS} {
		t.Run(transport, func(t *testing.T) {
			cfg := &Config{Transport: transport, Addr: "127.0.0.1:0", DialTimeout: time.Second}
			listener, err := listenWebSocket(cfg, quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			cfg.Addr = listener.Addr().String()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			conn, err := dialWebSocket(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			peer, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			cancel()
			conn.SetDeadline(time.Now().Add(time.Second))
			peer.SetDeadline(time.Now().Add(time.Second))
			if _, err := io.WriteString(conn, "live"); err != nil {
				t.Fatal(err)
			}
			var received [4]byte
			if _, err := io.ReadFull(peer, received[:]); err != nil || string(received[:]) != "live" {
				t.Fatalf("successful connection closed with dial context: %q, %v", received, err)
			}
		})
	}
}
