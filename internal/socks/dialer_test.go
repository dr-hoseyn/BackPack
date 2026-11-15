package socks

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPProxyDialCancellationReleasesHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	reached := make(chan struct{})
	closed := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		var greeting [3]byte
		if _, err := io.ReadFull(conn, greeting[:]); err != nil {
			closed <- err
			return
		}
		close(reached)
		_, err = conn.Read(greeting[:])
		closed <- err
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := HTTPClient(ln.Addr().String(), "", "", time.Second)
	transport := client.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	done := make(chan error, 1)
	go func() {
		conn, err := transport.DialContext(ctx, "tcp", "example.com:80")
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	select {
	case <-reached:
	case err := <-closed:
		t.Fatalf("proxy failed before greeting: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("proxy greeting not reached")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dial returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled dial retained a blocked SOCKS handshake")
	}
	select {
	case err := <-closed:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("proxy did not observe connection close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy connection remained open after cancellation")
	}
}

func TestSOCKSRejectsUnrepresentableRequestBeforeIO(t *testing.T) {
	for _, input := range []struct {
		user, pass, host string
		port             int
	}{
		{"", "", "", 80}, {"", "", strings.Repeat("h", 256), 80},
		{"", "", "example.com", 0}, {"", "", "example.com", 65536},
		{strings.Repeat("u", 256), "p", "example.com", 80},
		{"u", strings.Repeat("p", 256), "example.com", 80},
		{"u", "", "example.com", 80}, {"", "p", "example.com", 80},
	} {
		// nil deliberately detects any I/O before validation.
		if err := Negotiate(nil, input.user, input.pass, input.host, input.port); err == nil {
			t.Error("invalid request accepted")
		}
	}
}

func TestSOCKSConnectWireAddressTypes(t *testing.T) {
	for _, tc := range []struct {
		host    string
		atyp    byte
		address []byte
	}{
		{"example.com", atypHost, append([]byte{11}, "example.com"...)},
		{"192.0.2.1", atypIPv4, []byte{192, 0, 2, 1}},
		{"2001:db8::1", atypIPv6, net.ParseIP("2001:db8::1").To16()},
	} {
		t.Run(tc.host, func(t *testing.T) {
			client, proxy := net.Pipe()
			defer client.Close()
			defer proxy.Close()
			proxy.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan error, 1)
			go func() { done <- Negotiate(client, "", "", tc.host, 443) }()
			var greeting [3]byte
			if _, err := io.ReadFull(proxy, greeting[:]); err != nil {
				t.Fatal(err)
			}
			if _, err := proxy.Write([]byte{ver5, authNone}); err != nil {
				t.Fatal(err)
			}
			request := make([]byte, 4+len(tc.address)+2)
			if _, err := io.ReadFull(proxy, request); err != nil {
				t.Fatal(err)
			}
			want := append([]byte{ver5, cmdConn, 0, tc.atyp}, tc.address...)
			want = binary.BigEndian.AppendUint16(want, 443)
			if string(request) != string(want) {
				t.Fatalf("request %x, want %x", request, want)
			}
			if _, err := proxy.Write([]byte{ver5, repOK, 0, atypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSOCKSRejectsMalformedReplyHeaders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		auth  bool
		reply []byte
	}{
		{"auth-version", true, []byte{5, 0}},
		{"connect-version", false, []byte{4, 0, 0, atypIPv4}},
		{"connect-reserved", false, []byte{5, 0, 1, atypIPv4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, proxy := net.Pipe()
			defer client.Close()
			defer proxy.Close()
			proxy.SetDeadline(time.Now().Add(2 * time.Second))
			user, pass := "", ""
			method := byte(authNone)
			if tc.auth {
				user, pass, method = "u", "p", authUP
			}
			done := make(chan error, 1)
			go func() { done <- Negotiate(client, user, pass, "example.com", 443) }()
			var greeting [3]byte
			if _, err := io.ReadFull(proxy, greeting[:]); err != nil {
				t.Fatal(err)
			}
			if _, err := proxy.Write([]byte{ver5, method}); err != nil {
				t.Fatal(err)
			}
			requestSize := 4 + 1 + len("example.com") + 2
			if tc.auth {
				requestSize = 5
			} // version, ULEN, user, PLEN, pass
			if _, err := io.CopyN(io.Discard, proxy, int64(requestSize)); err != nil {
				t.Fatal(err)
			}
			if _, err := proxy.Write(tc.reply); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("malformed response accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("malformed response was not rejected before continuing negotiation")
			}
		})
	}
}

func TestSOCKSDialCancellationDoesNotCloseReturnedConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		var greeting [3]byte
		if _, err := io.ReadFull(conn, greeting[:]); err != nil {
			return
		}
		if _, err := conn.Write([]byte{ver5, authNone}); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(4+1+len("example.com")+2)); err != nil {
			return
		}
		if _, err := conn.Write([]byte{ver5, repOK, 0, atypIPv4, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		io.Copy(conn, conn)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := DialContext(ctx, ln.Addr().String(), "", "", "example.com", 443)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "live"); err != nil {
		t.Fatal(err)
	}
	var echoed [4]byte
	if _, err := io.ReadFull(conn, echoed[:]); err != nil || string(echoed[:]) != "live" {
		t.Fatalf("returned connection lost after cancellation: %q, %v", echoed, err)
	}
	conn.Close()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("proxy worker did not exit")
	}
}
