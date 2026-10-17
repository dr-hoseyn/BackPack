//go:build !windows

package network

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestMPTCPRequestStillTransfersToATCPOnlyPeer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan error, 1)
	go func() {
		c, e := l.Accept()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		_, e = io.Copy(c, c)
		done <- e
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := TcpDialerVia(ctx, &Outbound{MPTCP: true}, l.Addr().String(), time.Second, time.Second, true, 1, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("verified fallback")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len("verified fallback"))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "verified fallback" {
		t.Fatalf("payload %q: %v", b, err)
	}
	if !strings.Contains(MPTCPStatus(c), "TCP") {
		t.Fatal("negotiation status absent")
	}
	c.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMPTCPListenerAndDialerNegotiateWhenKernelSupportsIt(t *testing.T) {
	l, err := ListenWithBuffers("tcp", "127.0.0.1:0", 0, 0, 0, time.Second, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := TcpDialerVia(ctx, &Outbound{MPTCP: true}, l.Addr().String(), time.Second, time.Second, true, 1, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peer, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	got, err := c.MultipathTCP()
	if err != nil {
		t.Fatal(err)
	}
	other, err := peer.(*net.TCPConn).MultipathTCP()
	if err != nil || got != other {
		t.Fatalf("inconsistent negotiation: %v/%v: %v", got, other, err)
	}
	if !got {
		t.Skip("kernel did not negotiate MPTCP; TCP fallback verified separately")
	}
}
