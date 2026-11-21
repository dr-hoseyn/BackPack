package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func pilotListener(t *testing.T) *net.TCPListener {
	t.Helper()
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func pilotAccept(l *net.TCPListener, f func(*net.TCPConn)) {
	for {
		conn, err := l.AcceptTCP()
		if err != nil {
			return
		}
		go f(conn)
	}
}

func TestResumedStreamKeepsTheSameUserAndBackendSockets(t *testing.T) {
	backend := pilotListener(t)
	exit := pilotListener(t)
	proxy := pilotListener(t)
	entry := pilotListener(t)

	var backendOpens atomic.Int32
	go pilotAccept(backend, func(c *net.TCPConn) {
		defer c.Close()
		backendOpens.Add(1)
		_, _ = io.Copy(c, c)
	})
	key := []byte("a-test-only-hmac-key-at-least-32-bytes")
	relay := &relay{key: key, target: backend.Addr().(*net.TCPAddr), sessions: make(map[[16]byte]*stream)}
	go pilotAccept(exit, relay.accept)
	var attached atomic.Int32
	original := make(chan *net.TCPConn, 1)
	go pilotAccept(proxy, func(c *net.TCPConn) {
		defer c.Close()
		exitConn, err := connect(exit.Addr().(*net.TCPAddr), time.Second)
		if err != nil {
			return
		}
		defer exitConn.Close()
		if attached.Add(1) == 1 {
			original <- c
		}
		unwound := make(chan struct{})
		go func() {
			_, _ = io.Copy(exitConn, c)
			_ = exitConn.Close()
			close(unwound)
		}()
		_, _ = io.Copy(c, exitConn)
		_ = c.Close()
		<-unwound
	})
	go pilotAccept(entry, func(c *net.TCPConn) { edge(c, proxy.Addr().(*net.TCPAddr), key, false) })

	user, err := connect(entry.Addr().(*net.TCPAddr), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer user.Close()
	_ = user.SetDeadline(time.Now().Add(10 * time.Second))
	check := func(payload []byte) error {
		got := make([]byte, len(payload))
		send := make(chan error, 1)
		go func() {
			_, err := user.Write(payload)
			send <- err
		}()
		_, err := io.ReadFull(user, got)
		if err != nil {
			return err
		}
		if err := <-send; err != nil {
			return err
		}
		if !bytes.Equal(got, payload) {
			return fmt.Errorf("payload differed after transferring %d bytes", len(payload))
		}
		return nil
	}
	if err := check([]byte("open and verify the original carrier")); err != nil {
		t.Fatal(err)
	}
	first := <-original
	after := bytes.Repeat([]byte("continuing the same byte stream"), 80000)
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = first.Close()
	}()
	if err := check(after); err != nil {
		t.Fatal(err)
	}
	if err := user.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if n, err := user.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("expected propagated direction close, got %d bytes / %v", n, err)
	}
	if n := backendOpens.Load(); n != 1 {
		t.Fatalf("backend was opened %d times", n)
	}
	if n := attached.Load(); n != 2 {
		t.Fatalf("expected original and replacement carriers, observed %d", n)
	}
}
