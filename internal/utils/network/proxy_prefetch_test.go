package network

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

func serverFirstProxy(t *testing.T, status int, greeting string, interim ...int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		// A single write coalesces the response and the peer's first bytes.
		for _, code := range interim {
			fmt.Fprintf(conn, "HTTP/1.1 %d Interim\r\n\r\n", code)
		}
		fmt.Fprintf(conn, "HTTP/1.1 %d Connected\r\nContent-Length: 999999\r\n\r\n%s", status, greeting)
		io.Copy(conn, conn)
	}()
	t.Cleanup(func() {
		ln.Close()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("proxy fixture did not stop")
		}
	})
	return ln.Addr().String()
}

func dialServerFirstProxy(t *testing.T, status int, greeting string, interim ...int) net.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := TcpDialerVia(ctx, &Outbound{Proxy: &ProxyConfig{Scheme: "http", Address: serverFirstProxy(t, status, greeting, interim...)}},
		"tunnel.example:9000", time.Second, time.Second, true, 1, 0, 0, 0)
	if err != nil {
		t.Fatalf("CONNECT status %d: %v", status, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestHTTPConnectPreservesServerFirstBytes(t *testing.T) {
	const greeting = "SSH-2.0-BackPackTest\r\n"
	for _, status := range []int{200, 201, 204, 299} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			conn := dialServerFirstProxy(t, status, greeting)
			conn.SetDeadline(time.Now().Add(time.Second))
			buf := make([]byte, len(greeting))
			if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != greeting {
				t.Fatalf("server-first bytes lost: %q, %v", buf, err)
			}
			if _, err := conn.Write([]byte("ping")); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(conn, buf[:4]); err != nil || string(buf[:4]) != "ping" {
				t.Fatalf("tunnel after greeting: %q, %v", buf[:4], err)
			}
		})
	}
}

func TestHTTPConnectBufferedBytesHonorReadState(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		conn := dialServerFirstProxy(t, 200, "greeting")
		conn.SetReadDeadline(time.Now().Add(-time.Second))
		buf := make([]byte, len("greeting"))
		if _, err := conn.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("buffered read bypassed deadline: %v", err)
		}
		conn.SetReadDeadline(time.Time{})
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "greeting" {
			t.Fatalf("deadline consumed greeting: %q, %v", buf, err)
		}
	})
	t.Run("close", func(t *testing.T) {
		conn := dialServerFirstProxy(t, 200, "greeting")
		conn.Close()
		if _, err := conn.Read(make([]byte, 32)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("closed proxy returned buffered data: %v", err)
		}
	})
}

func TestHTTPConnectWithoutPrefetchKeepsTCPConnection(t *testing.T) {
	conn := dialServerFirstProxy(t, 200, "")
	if _, ok := conn.(*net.TCPConn); !ok {
		t.Fatalf("header-only CONNECT unnecessarily wrapped %T", conn)
	}
}

func TestHTTPConnectReadsFinalStatusAfterInformationalResponses(t *testing.T) {
	conn := dialServerFirstProxy(t, 200, "greeting", 100, 103)
	conn.SetDeadline(time.Now().Add(time.Second))
	buf := make([]byte, len("greeting"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "greeting" {
		t.Fatalf("interim response confused tunnel bytes: %q, %v", buf, err)
	}
}

func TestHTTPConnectPrefetchPreservesTCPHalfClose(t *testing.T) {
	conn := dialServerFirstProxy(t, 200, "greeting")
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	writer, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("proxied TCP lost directional EOF: %T", conn)
	}
	if err := writer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "greetingrequest" {
		t.Fatalf("prefetched and live bytes across FIN: %q, %v", got, err)
	}
}

// net.Pipe deterministically puts the HTTP headers and tunnel bytes in one
// underlying read. The stream exceeds the header limit and is read in small
// pieces, exercising both the saved prefix and subsequent unbounded I/O.
func TestHTTPConnectPrefetchSurvivesSmallReadsAndHeaderBudget(t *testing.T) {
	client, proxy := net.Pipe()
	defer client.Close()
	defer proxy.Close()
	proxy.SetDeadline(time.Now().Add(2 * time.Second))
	type connected struct {
		conn net.Conn
		err  error
	}
	ready := make(chan connected, 1)
	go func() {
		conn, err := httpConnect(client, &ProxyConfig{}, "example.com:443", time.Second)
		ready <- connected{conn, err}
	}()
	if _, err := http.ReadRequest(bufio.NewReader(proxy)); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("a tunnel byte sequence"), 8192)
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(proxy, "HTTP/1.1 200 Connected\r\n\r\n"+string(want))
		written <- err
	}()
	result := <-ready
	if result.err != nil {
		t.Fatal(result.err)
	}
	result.conn.SetDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(want))
	for offset := 0; offset < len(got); offset += 37 {
		if _, err := io.ReadFull(result.conn, got[offset:min(offset+37, len(got))]); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal("CONNECT prefetch lost or duplicated tunnel bytes")
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}
