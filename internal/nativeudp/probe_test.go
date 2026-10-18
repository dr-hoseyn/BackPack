package nativeudp

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestNativePathProofDoesNotReplaceTheActiveNoiseSession(t *testing.T) {
	s, c, _, peer := linkPair(t)
	s.mu.Lock()
	id := s.active.id
	s.mu.Unlock()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := Probe(ctx, pc, peer, "private-native-udp-token-for-tests"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	same := id == s.active.id
	s.mu.Unlock()
	if !same {
		t.Fatal("health probe replaced the active client")
	}
	if err := c.Write([]byte("still active")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 100)
	n, err := s.Read(b)
	if err != nil || string(b[:n]) != "still active" {
		t.Fatal("health probe interrupted live datagrams")
	}
}

func TestReflectedNativeProbeIsNotAcceptedAsAServerProof(t *testing.T) {
	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	go func() {
		b := make([]byte, 2000)
		n, a, e := server.ReadFrom(b)
		if e == nil {
			b[0] = 5
			server.WriteTo(b[:n], a)
		}
	}()
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := Probe(ctx, client, server.LocalAddr(), "private-native-udp-token-for-tests"); err == nil {
		t.Fatal("a reflected client proof impersonated the server")
	}
}
