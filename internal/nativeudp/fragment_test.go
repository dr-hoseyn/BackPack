package nativeudp

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestLargeNativeDatagramsReassembleOutOfOrderWithoutBlockingLaterPackets(t *testing.T) {
	s, c, record, peer := linkPair(t)
	record.mu.Lock()
	record.hold = true
	record.mu.Unlock()
	want := bytes.Repeat([]byte("fragmented authenticated packet;"), 1800)
	if err := c.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := c.Write([]byte("later independent datagram")); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 2)
	go func() {
		b := make([]byte, maxPayload)
		for range 2 {
			n, err := s.Read(b)
			if err != nil {
				return
			}
			got <- append([]byte(nil), b[:n]...)
		}
	}()
	select {
	case p := <-got:
		if string(p) != "later independent datagram" {
			t.Fatal("incomplete packet blocked or corrupted a later message")
		}
	case <-time.After(time.Second):
		t.Fatal("head-of-line wait on incomplete packet")
	}
	record.mu.Lock()
	parts := record.packets
	record.hold = false
	record.packets = nil
	record.mu.Unlock()
	if len(parts) < 2 {
		t.Fatal("large message was not fragmented")
	}
	for i := len(parts) - 1; i >= 0; i-- {
		if _, err := record.PacketConn.WriteTo(parts[i], peer); err != nil {
			t.Fatal(err)
		}
		if i == len(parts)-1 {
			record.PacketConn.WriteTo(parts[i], peer)
		} // duplicate must not count twice
		time.Sleep(time.Millisecond)
	}
	select {
	case p := <-got:
		if !bytes.Equal(p, want) {
			t.Fatal("out-of-order reassembly changed datagram boundary or bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("complete unordered packet not delivered")
	}
	s.mu.Lock()
	pending := len(s.assemblies)
	s.mu.Unlock()
	if pending != 0 {
		t.Fatal("completed reassembly state retained")
	}
}

func TestIncompleteNativeDatagramExpiresWithoutGrowingState(t *testing.T) {
	s, c, record, peer := linkPair(t)
	record.mu.Lock()
	record.hold = true
	record.mu.Unlock()
	if err := c.Write(bytes.Repeat([]byte{9}, 3000)); err != nil {
		t.Fatal(err)
	}
	record.mu.Lock()
	part := append([]byte(nil), record.packets[0]...)
	record.hold = false
	record.mu.Unlock()
	if _, err := record.PacketConn.WriteTo(part, peer); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		n := len(s.assemblies)
		for _, a := range s.assemblies {
			a.created = time.Now().Add(-assemblyLife - time.Second)
		}
		s.mu.Unlock()
		if n != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fragment did not reach reassembly")
		}
		time.Sleep(time.Millisecond)
	}
	deadline = time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		n := len(s.assemblies)
		s.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("expired partial packet retained")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Write([]byte("still live")); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { b := make([]byte, 100); n, _ := s.Read(b); done <- string(b[:n]) }()
	select {
	case p := <-done:
		if p != "still live" {
			t.Fatal(p)
		}
	case <-time.After(time.Second):
		t.Fatal("expired fragment blocked the association")
	}
}

func TestFullNativeUDPReassemblyContinuesDeliveringNewDatagrams(t *testing.T) {
	link := &Link{assemblies: make(map[fragmentKey]*assembly)}
	session := [16]byte{1}
	payload := bytes.Repeat([]byte{0x42}, fragmentSize+200)
	fragment := func(id uint64, index uint16) []byte {
		part := make([]byte, fragmentHeader)
		part[0] = 3
		binary.BigEndian.PutUint64(part[1:9], id)
		binary.BigEndian.PutUint16(part[9:11], index)
		binary.BigEndian.PutUint16(part[11:13], 2)
		binary.BigEndian.PutUint32(part[13:17], uint32(len(payload)))
		offset := int(index) * fragmentSize
		return append(part, payload[offset:min(len(payload), offset+fragmentSize)]...)
	}
	for id := uint64(1); id <= assemblyLimit; id++ {
		if link.assemble(session, fragment(id, 0)) != nil {
			t.Fatal("partial datagram delivered")
		}
	}
	const newer = assemblyLimit + 1
	// Use a different payload in the recycled slot to detect stale bytes.
	for i := range payload {
		payload[i] = byte(i)
	}
	if link.assemble(session, fragment(newer, 1)) != nil {
		t.Fatal("partial out-of-order datagram delivered")
	}
	if _, exists := link.assemblies[fragmentKey{session, 1}]; exists {
		t.Fatal("oldest incomplete datagram still retained after pressure")
	}
	if len(link.assemblies) != assemblyLimit {
		t.Fatal("reassembly exceeded its original bound")
	}
	got := link.assemble(session, fragment(newer, 0))
	if !bytes.Equal(got, payload) {
		t.Fatal("new complete datagram blocked by old incomplete datagrams")
	}
	if got := link.dropped.Load(); got != 1 {
		t.Fatalf("dropped %d datagrams, want only the evicted one", got)
	}

	// Normal completion removes a node from anywhere in the order.
	link.assemble(session, fragment(10, 1))
	if len(link.assemblies) != assemblyLimit-2 || link.oldestAssembly.key.message != 2 || link.newestAssembly.key.message != assemblyLimit {
		t.Fatal("reassembly order inconsistent after interior completion")
	}
}
