package naive

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func socksPacketReceiver(t testing.TB) (*SOCKSPacketConn, *net.UDPConn) {
	t.Helper()
	listen := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	local, relay := listen(), listen()
	// Queue forged and fragmented jumbo packets before the valid packet so
	// source/header validation cannot pass by simply accepting the first read.
	if err := local.SetReadBuffer(1024 * 1024); err != nil {
		t.Fatal(err)
	}
	// Exercise the PacketConn read seam without a helper process. Association
	// setup/restarts are covered by the existing pinned sing-box tests.
	c := &SOCKSPacketConn{
		UDPConn: local, ctx: t.Context(), relay: relay.LocalAddr().(*net.UDPAddr),
		target: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53},
		header: []byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 53},
	}
	return c, relay
}

func TestSOCKSPacketValueReadsPreservePeerChecksAndDatagramBoundaries(t *testing.T) {
	c, relay := socksPacketReceiver(t)
	wrongPeer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer wrongPeer.Close()
	peer := c.LocalAddr().(*net.UDPAddr).AddrPort()
	for _, size := range []int{64, 0, 1200, 60000, 7} {
		body := bytes.Repeat([]byte{byte(size)}, size)
		wire := append(bytes.Clone(c.header), body...)
		if _, err := wrongPeer.WriteToUDPAddrPort(wire, peer); err != nil {
			t.Fatal(err)
		}
		fragment := bytes.Clone(wire)
		fragment[2] = 1
		if _, err := relay.WriteToUDPAddrPort(fragment, peer); err != nil {
			t.Fatal(err)
		}
		if _, err := relay.WriteToUDPAddrPort(wire, peer); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 65535)
		n, target, err := c.ReadFrom(buf)
		if err != nil || target != c.target || !bytes.Equal(buf[:n], body) {
			t.Fatalf("%d-byte SOCKS packet: n=%d peer=%v error=%v", size, n, target, err)
		}
	}
}

func BenchmarkSOCKSPacketReceive(b *testing.B) {
	c, relay := socksPacketReceiver(b)
	peer := c.LocalAddr().(*net.UDPAddr).AddrPort()
	wire := append(bytes.Clone(c.header), make([]byte, 1200)...)
	buf := make([]byte, 65535)
	_ = c.SetReadDeadline(time.Now().Add(time.Hour))
	b.SetBytes(1200)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := relay.WriteToUDPAddrPort(wire, peer); err != nil {
			b.Fatal(err)
		}
		n, _, err := c.ReadFrom(buf)
		if err != nil || n != 1200 {
			b.Fatalf("SOCKS datagram: n=%d error=%v", n, err)
		}
	}
}
