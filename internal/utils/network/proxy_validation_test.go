package network

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProxyValidationDoesNotLeakCredentials(t *testing.T) {
	for _, raw := range []string{
		"http://user:secret-password@127.0.0.1",
		"http://user:secret-password@",
		"http://user:secret-password@127.0.0.1:bad",
		"http://user:secret-password@%zz:8080",
	} {
		_, err := ParseProxy(raw)
		if err == nil {
			t.Errorf("accepted malformed proxy URL")
		} else if strings.Contains(err.Error(), "secret-password") {
			t.Errorf("validation error leaked credentials: %v", err)
		}
	}
}

func TestProxyValidationRejectsUnusableSettings(t *testing.T) {
	for _, raw := range []string{
		"http://:8080", "http://localhost:", "http://localhost:0",
		"http://localhost:65536", "http://localhost:8080/path",
		"http://localhost:8080?token=value", "http://localhost:8080#fragment",
		"socks5://" + strings.Repeat("u", 256) + ":pass@localhost:1080",
		"socks5://user:" + strings.Repeat("p", 256) + "@localhost:1080",
		"socks5://user@localhost:1080", "socks5://:pass@localhost:1080",
	} {
		if _, err := ParseProxy(raw); err == nil {
			t.Errorf("accepted unusable proxy settings")
		}
	}
	if _, err := ParseProxy("socks5://user:pass@[::1]:1080/"); err != nil {
		t.Fatalf("valid IPv6 proxy rejected: %v", err)
	}
}

func TestTCPDialerMakesAtLeastOneAttempt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, retries := range []int{0, -1} {
		conn, err := TcpDialer(context.Background(), ln.Addr().String(), time.Second, time.Second, true, retries, 0, 0, 0)
		if err != nil || conn == nil {
			t.Fatalf("retries=%d: conn=%v err=%v", retries, conn, err)
		}
		conn.Close()
		peer, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		peer.Close()
	}
}

type optionRawConn struct{ accessErr error }

func (r optionRawConn) Control(f func(uintptr)) error {
	if r.accessErr != nil {
		return r.accessErr
	}
	f(0)
	return nil
}
func (optionRawConn) Read(func(uintptr) bool) error  { panic("unused") }
func (optionRawConn) Write(func(uintptr) bool) error { panic("unused") }

func TestSocketControlPreservesFailures(t *testing.T) {
	want := errors.New("socket option rejected")
	if err := controlSocket(optionRawConn{}, func(uintptr) error { return want }); !errors.Is(err, want) {
		t.Fatalf("option error lost: %v", err)
	}
	if err := controlSocket(optionRawConn{accessErr: want}, func(uintptr) error {
		t.Fatal("callback called after descriptor access failed")
		return nil
	}); !errors.Is(err, want) {
		t.Fatalf("descriptor access error lost: %v", err)
	}
}

// Rejected responses are not reusable HTTP connections. Draining an advertised
// body would delay failover, even after the refusal has already been received.
func TestHTTPConnectDoesNotDrainResponseBodies(t *testing.T) {
	for _, status := range []string{"407 Proxy Authentication Required", "200 Connection Established"} {
		t.Run(status[:3], func(t *testing.T) {
			client, proxy := net.Pipe()
			defer client.Close()
			defer proxy.Close()
			proxy.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan error, 1)
			go func() { done <- httpConnect(client, &ProxyConfig{}, "example.com:443", time.Second) }()
			if _, err := http.ReadRequest(bufio.NewReader(proxy)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(proxy, "HTTP/1.1 "+status+"\r\nContent-Length: 999999\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if strings.HasPrefix(status, "200") && err != nil {
					t.Fatalf("successful CONNECT failed: %v", err)
				}
				if strings.HasPrefix(status, "407") && (err == nil || !strings.Contains(err.Error(), "407")) {
					t.Fatalf("refusal lost: %v", err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("CONNECT drained an HTTP body instead of returning its status")
			}
		})
	}
}

func TestHTTPConnectBoundsResponseHeaders(t *testing.T) {
	client, proxy := net.Pipe()
	defer client.Close()
	defer proxy.Close()
	proxy.SetDeadline(time.Now().Add(2 * time.Second))
	done := make(chan error, 1)
	go func() {
		defer client.Close()
		done <- httpConnect(client, &ProxyConfig{}, "example.com:443", time.Second)
	}()
	if _, err := http.ReadRequest(bufio.NewReader(proxy)); err != nil {
		t.Fatal(err)
	}
	io.WriteString(proxy, "HTTP/1.1 200 Connection Established\r\nX-Large: "+strings.Repeat("a", 128<<10)+"\r\n\r\n")
	if err := <-done; err == nil || !strings.Contains(err.Error(), "headers exceed") {
		t.Fatalf("oversized CONNECT response was not rejected: %v", err)
	}
}
