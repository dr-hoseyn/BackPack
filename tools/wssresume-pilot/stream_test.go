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
	carriers := make(chan *net.TCPConn, 3)
	go pilotAccept(proxy, func(c *net.TCPConn) {
		defer c.Close()
		exitConn, err := connect(exit.Addr().(*net.TCPAddr), time.Second)
		if err != nil {
			return
		}
		defer exitConn.Close()
		attached.Add(1)
		carriers <- c
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
	after := bytes.Repeat([]byte("continuing the same byte stream"), 80000)
	for outage := range 2 {
		var carrier *net.TCPConn
		select {
		case carrier = <-carriers:
		case <-time.After(2 * time.Second):
			t.Fatalf("missing carrier for outage %d", outage+1)
		}
		timer := time.AfterFunc(5*time.Millisecond, func() { _ = carrier.Close() })
		if err := check(after); err != nil {
			timer.Stop()
			t.Fatal(err)
		}
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
	if n := attached.Load(); n != 3 {
		t.Fatalf("expected original and two replacement carriers, observed %d", n)
	}
}

func TestResumeExpiredCarrierReleasesBackend(t *testing.T) {
	backend := pilotListener(t)
	exit := pilotListener(t)
	key := []byte("a-test-only-hmac-key-at-least-32-bytes")
	r := &relay{key: key, target: backend.Addr().(*net.TCPAddr), sessions: make(map[[16]byte]*stream)}
	go pilotAccept(exit, r.accept)

	carrier, err := connect(exit.Addr().(*net.TCPAddr), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer carrier.Close()
	_ = carrier.SetDeadline(time.Now().Add(3 * time.Second))
	req := handshake{id: [16]byte{1}, nonce: [16]byte{2}, flags: openFlag}
	if err := req.write(carrier, key, 'Q'); err != nil {
		t.Fatal(err)
	}
	if _, err := readHandshake(carrier, key, 'R'); err != nil {
		t.Fatal(err)
	}

	_ = backend.SetDeadline(time.Now().Add(time.Second))
	app, err := backend.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	disconnected := time.Now()
	_ = carrier.Close()
	_ = app.SetReadDeadline(disconnected.Add(resumeTimeout + 2*time.Second))
	if n, err := app.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("backend was not released on resume expiry: %d bytes / %v", n, err)
	}
	if time.Since(disconnected) < resumeTimeout-time.Second {
		t.Fatal("backend closed before the resume window")
	}
	late, err := connect(exit.Addr().(*net.TCPAddr), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	_ = late.SetDeadline(time.Now().Add(time.Second))
	req.nonce[0]++
	req.flags = 0
	if err := req.write(late, key, 'Q'); err != nil {
		t.Fatal(err)
	}
	if _, err := readHandshake(late, key, 'R'); err != io.EOF {
		t.Fatalf("expired session accepted a resume or kept the carrier open: %v", err)
	}
}
