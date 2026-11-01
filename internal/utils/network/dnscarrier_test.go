package network

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestDNSCarrierRoundTripsBinaryAndIgnoresUnrelatedPackets(t *testing.T) {
	a, err := newDNSPacketConn("127.0.0.1:0", "secret", false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := newDNSPacketConn("127.0.0.1:0", "secret", true)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	a.SetDeadline(time.Now().Add(time.Second))
	b.SetDeadline(time.Now().Add(time.Second))
	stray, err := net.Dial("udp", b.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	stray.Write([]byte("not DNS"))
	stray.Close()
	payload := bytes.Repeat([]byte{0, 1, 127, 128, 255}, 200)
	if _, err := a.WriteTo(payload, b.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	n, from, err := b.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("query payload changed: %v", err)
	}
	b.WriteTo(buf[:n], from)
	n, _, err = a.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("response payload changed: %v", err)
	}
	if _, err := a.WriteTo(make([]byte, dnsCarrierMTU+1), b.LocalAddr()); err == nil {
		t.Fatal("oversized DNS payload accepted")
	}
	a.WriteTo(payload, b.LocalAddr())
	if _, _, err := b.ReadFrom(make([]byte, 1)); err != io.ErrShortBuffer {
		t.Fatal("truncated payload was accepted")
	}
}
