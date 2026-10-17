package nativeudp

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

type recordedConn struct {
	net.PacketConn
	mu     sync.Mutex
	packet []byte
	drop   bool
}

func (c *recordedConn) WriteTo(b []byte, a net.Addr) (int, error) {
	c.mu.Lock()
	if len(b) > 100 && b[0] == 3 {
		c.packet = append(c.packet[:0], b...)
		if c.drop {
			c.drop = false
			c.mu.Unlock()
			return len(b), nil
		}
	}
	c.mu.Unlock()
	return c.PacketConn.WriteTo(b, a)
}

func linkPair(t *testing.T) (*Link, *Link, *recordedConn, net.Addr) {
	t.Helper()
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	record := &recordedConn{PacketConn: client}
	s, err := NewLink(t.Context(), socket, nil, "private-native-udp-token-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := NewLink(t.Context(), record, socket.LocalAddr(), "private-native-udp-token-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	deadline := time.Now().Add(4 * time.Second)
	for !c.Stats().Connected || !s.Stats().Connected {
		if time.Now().After(deadline) {
			t.Fatal("Noise handshake did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s, c, record, socket.LocalAddr()
}

func TestNativeDatagramLossDoesNotBlockOrRetransmitALaterPacket(t *testing.T) {
	s, c, record, _ := linkPair(t)
	record.mu.Lock()
	record.drop = true
	record.mu.Unlock()
	if err := c.Write(bytes.Repeat([]byte{1}, 1200)); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{2}, 1200)
	if err := c.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 2)
	go func() {
		b := make([]byte, 1500)
		for {
			n, err := s.Read(b)
			if err != nil {
				return
			}
			got <- append([]byte(nil), b[:n]...)
		}
	}()
	select {
	case b := <-got:
		if !bytes.Equal(b, want) {
			t.Fatal("lost packet was delivered or later payload corrupted")
		}
	case <-time.After(time.Second):
		t.Fatal("later datagram blocked behind loss")
	}
	select {
	case <-got:
		t.Fatal("user datagram was retransmitted")
	case <-time.After(650 * time.Millisecond):
	}
}

func TestNativeDatagramAuthenticationRejectsReplaysAndTamperedCounters(t *testing.T) {
	s, c, record, peer := linkPair(t)
	if err := c.Write(bytes.Repeat([]byte{9}, 200)); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 500)
	if _, err := s.Read(b); err != nil {
		t.Fatal(err)
	}
	record.mu.Lock()
	packet := append([]byte(nil), record.packet...)
	record.mu.Unlock()
	if _, err := record.PacketConn.WriteTo(packet, peer); err != nil {
		t.Fatal(err)
	}
	packet[17] ^= 0x40
	if _, err := record.PacketConn.WriteTo(packet, peer); err != nil {
		t.Fatal(err)
	}
	got := make(chan int, 1)
	go func() { n, _ := s.Read(b); got <- n }()
	select {
	case <-got:
		t.Fatal("replayed or forged datagram accepted")
	case <-time.After(150 * time.Millisecond):
	}
	if err := c.Write([]byte("next authentic packet")); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if string(b[:n]) != "next authentic packet" {
			t.Fatal("forgery advanced replay window")
		}
	case <-time.After(time.Second):
		t.Fatal("authentic traffic stopped after forgery")
	}
}

func TestNativeReverseUDPForwardsIndependentFlowsAndReleasesSockets(t *testing.T) {
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		b := make([]byte, 65535)
		for {
			n, a, e := backend.ReadFrom(b)
			if e != nil {
				return
			}
			backend.WriteTo(b[:n], a)
		}
	}()
	public, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entry := public.LocalAddr().String()
	public.Close()
	socket, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		socket.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := StartServer(ctx, socket, "private-native-udp-token-for-tests", []string{entry + "=" + backend.LocalAddr().String()}, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := StartClient(ctx, cs, socket.LocalAddr(), "private-native-udp-token-for-tests", 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for !s.Stats().Connected {
		if time.Now().After(deadline) {
			t.Fatal("native path did not authenticate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := range 4 {
		user, err := net.Dial("udp", entry)
		if err != nil {
			t.Fatal(err)
		}
		want := bytes.Repeat([]byte{byte(i)}, 1000)
		user.SetDeadline(time.Now().Add(time.Second))
		user.Write(want)
		b := make([]byte, 1200)
		n, err := user.Read(b)
		user.Close()
		if err != nil || !bytes.Equal(b[:n], want) {
			t.Fatalf("flow %d corrupted: %d %v", i, n, err)
		}
	}
	if s.Stats().Flows != 4 || c.Stats().Flows != 4 {
		t.Fatal("flow accounting missing")
	}
	user, err := net.Dial("udp", entry)
	if err != nil {
		t.Fatal(err)
	}
	user.SetDeadline(time.Now().Add(80 * time.Millisecond))
	user.Write([]byte("over limit"))
	_, err = user.Read(make([]byte, 100))
	user.Close()
	if err == nil {
		t.Fatal("native flow limit bypassed")
	}
	cancel()
	s.Close()
	c.Close()
	bound, err := net.ListenPacket("udp", entry)
	if err != nil {
		t.Fatal("UDP listener survived shutdown:", err)
	}
	bound.Close()
}
