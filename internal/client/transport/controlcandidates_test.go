package transport

import (
	"context"
	"github.com/backpack/backpack/internal/utils"
	"github.com/backpack/backpack/internal/utils/network"
	"net"
	"testing"
	"time"
)

func TestReviewedControlCandidatesTryAllHandshakesBeforeRepeating(t *testing.T) {
	e := network.NewEndpoints("primary", "second", "third")
	var failed map[string]bool
	for _, bad := range []string{"primary", "second"} {
		rejectControlCandidate(&failed, bad)
		for _, a := range controlCandidates(e, &failed) {
			if failed[a] {
				t.Fatal("failed handshake remained eligible")
			}
		}
	}
	if got := controlCandidates(e, &failed); len(got) != 1 || got[0] != "third" {
		t.Fatalf("candidates %v", got)
	}
	rejectControlCandidate(&failed, "third")
	if got := controlCandidates(e, &failed); len(got) != 3 || failed != nil {
		t.Fatal("completed sweep did not reset")
	}
}

func TestReviewedTCPClientReachesBackupAfterAcceptedHandshakesClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var listeners []net.Listener
	for i := 0; i < 3; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
		defer ln.Close()
	}
	for _, ln := range listeners[:2] {
		go func(ln net.Listener) {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				conn.Close()
			}
		}(ln)
	}
	ready := make(chan struct{})
	go func() {
		conn, err := listeners[2].Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		token, _, err := utils.ReceiveBinaryTransportString(conn)
		if err != nil || token != "test-token" {
			return
		}
		if utils.SendBinaryTransportStringWithin(conn, token, utils.SG_Chan, time.Second) != nil {
			return
		}
		close(ready)
		<-ctx.Done()
	}()
	endpoints := network.NewEndpoints(listeners[0].Addr().String(), listeners[1].Addr().String(), listeners[2].Addr().String())
	client := NewTCPClient(ctx, &TcpConfig{Endpoints: endpoints, Token: "test-token", DialTimeOut: time.Second, RetryInterval: time.Millisecond, KeepAlive: time.Second}, silentLogger())
	defer func() { cancel(); client.state.Stop(); client.state.Wait() }()
	client.Start()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("TCP-only winners prevented backup handshake")
	}
	for !client.Running() {
		select {
		case <-ctx.Done():
			t.Fatal("backup never became connected")
		case <-time.After(time.Millisecond):
		}
	}
	if endpoints.Current() != listeners[2].Addr().String() {
		t.Fatal("pool did not follow authenticated backup")
	}
}
