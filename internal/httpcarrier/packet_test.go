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
		if got := p.reassemble("peer", frame(uint32(i), 0), now.Add(time.Duration(i)*time.Nanosecond)); got != nil {
			t.Fatal("partial packet delivered")
		}
	}
	if len(p.fragments) != 32 {
		t.Fatalf("unbounded packet assemblies: %d", len(p.fragments))
	}
	at := now.Add(time.Microsecond)
	if got := p.reassemble("peer", frame(999, 0), at); got != nil {
		t.Fatal("duplicate completed a packet")
	}
	if got := p.reassemble("another-peer", frame(999, 1), at); got != nil {
		t.Fatal("cross-peer fragment accepted")
	}
	got := p.reassemble("peer", frame(999, 1), at)
	if len(got) != 2000 || got[0] != 1 || got[1000] != 2 {
		t.Fatal("out-of-order reconstruction failed")
	}
	p.reassemble("peer", frame(1001, 0), now.Add(6*time.Second))
	if len(p.fragments) != 1 {
		t.Fatal("expired partial packets retained")
	}
}

// Missing fragments must not reserve every slot for the entire five-second
// expiry. Fresh complete packets must still make progress after a loss burst.
func TestWebTransportReassemblyRecoversWhenLostFragmentsFillCapacity(t *testing.T) {
	p := newPackets(context.Background())
	defer p.cancel()
	now := time.Now()
	frame := func(id uint32, index byte) []byte {
		b := make([]byte, fragmentHeader+fragmentPayload)
		b[0], b[1], b[2] = 1, 2, index
		binary.BigEndian.PutUint32(b[4:8], id)
		binary.BigEndian.PutUint16(b[8:10], 2000)
		for i := fragmentHeader; i < len(b); i++ {
			b[i] = index + 1
		}
		return b
	}
	for i := range 32 {
		if got := p.reassemble("peer", frame(uint32(i), 0), now.Add(time.Duration(i)*time.Nanosecond)); got != nil {
			t.Fatal("incomplete packet delivered")
		}
	}
	at := now.Add(time.Millisecond)
	if got := p.reassemble("peer", frame(32, 1), at); got != nil {
		t.Fatal("incomplete fresh packet delivered")
	}
	got := p.reassemble("peer", frame(32, 0), at)
	if len(got) != 2000 || !bytes.Equal(got[:1000], bytes.Repeat([]byte{1}, 1000)) || !bytes.Equal(got[1000:], bytes.Repeat([]byte{2}, 1000)) {
		t.Fatal("lost fragments blocked a fresh complete packet until expiry")
	}
	if len(p.fragments) > 32 {
		t.Fatalf("unbounded packet assemblies: %d", len(p.fragments))
	}
}
