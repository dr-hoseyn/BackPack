package httpcarrier

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestWebTransportDatagramsPreserveLargePacketsAndDeadlines(t *testing.T) {
	st, ct := tlsPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := ListenPackets(ctx, "127.0.0.1:0", "/private-datagrams", "backpack", "separate-secret-123456", st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := DialPackets(ctx, ctx, s.LocalAddr().String(), "/private-datagrams", "backpack", "separate-secret-123456", ct)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, size := range []int{1, 1280, 1500, 8192} {
		payload := make([]byte, size)
		rand.Read(payload)
		if n, err := c.WriteTo(payload, c.Peer()); err != nil || n != len(payload) {
			t.Fatalf("send %d: n=%d err=%v", size, n, err)
		}
		s.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, MaxDatagram)
		n, peer, err := s.ReadFrom(buf)
		if err != nil || !bytes.Equal(buf[:n], payload) {
			t.Fatalf("receive %d: n=%d err=%v", size, n, err)
		}
		if _, err := s.WriteTo(buf[:n], peer); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _, err = c.ReadFrom(buf)
		if err != nil || !bytes.Equal(buf[:n], payload) {
			t.Fatalf("reply %d: n=%d err=%v", size, n, err)
		}
	}
	if _, err := c.WriteTo([]byte("wrong peer"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}); err == nil {
		t.Fatal("arbitrary datagram destination accepted")
	}
	c.SetReadDeadline(time.Time{})
	done := make(chan error, 1)
	go func() { _, _, err := c.ReadFrom(make([]byte, 64)); done <- err }()
	c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("deadline change did not interrupt pending read")
	}
	c.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := c.WriteTo([]byte("expired"), c.Peer()); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write deadline: %v", err)
	}
	c.Close()
	if _, _, err := c.ReadFrom(make([]byte, 64)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
}

func TestWebTransportReassemblyIsBoundedAndDropsMissingFragments(t *testing.T) {
	p := newPackets(context.Background())
	defer p.cancel()
	now := time.Now()
	frame := func(id uint32, index int) []byte {
		b := make([]byte, fragmentHeader+fragmentPayload)
		b[0], b[1], b[2] = 1, 2, byte(index)
		binary.BigEndian.PutUint32(b[4:8], id)
		binary.BigEndian.PutUint16(b[8:10], 2000)
		for i := fragmentHeader; i < len(b); i++ {
			b[i] = byte(index + 1)
		}
		return b
	}
	for i := range 1000 {
		if got := p.reassemble("peer", frame(uint32(i), 0), now); got != nil {
			t.Fatal("partial packet delivered")
		}
	}
	if len(p.fragments) != 32 {
		t.Fatalf("unbounded packet assemblies: %d", len(p.fragments))
	}
	if got := p.reassemble("peer", frame(0, 0), now); got != nil {
		t.Fatal("duplicate completed a packet")
	}
	if got := p.reassemble("another-peer", frame(0, 1), now); got != nil {
		t.Fatal("cross-peer fragment accepted")
	}
	got := p.reassemble("peer", frame(0, 1), now)
	if len(got) != 2000 || got[0] != 1 || got[1000] != 2 {
		t.Fatal("out-of-order reconstruction failed")
	}
	p.reassemble("peer", frame(1001, 0), now.Add(6*time.Second))
	if len(p.fragments) != 1 {
		t.Fatal("expired partial packets retained")
	}
}
