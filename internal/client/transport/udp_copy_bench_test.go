package transport

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// Include both real socket reads and the forwarding worker, so an unused
// source-address allocation in udpCopy appears in the per-datagram count.
func BenchmarkRawUDPConnectedCopy(b *testing.B) {
	listen := func() *net.UDPConn {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { c.Close() })
		return c
	}
	sender, receiver := listen(), listen()
	dial := func(peer *net.UDPConn) *net.UDPConn {
		c, err := net.DialUDP("udp4", nil, peer.LocalAddr().(*net.UDPAddr))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { c.Close() })
		return c
	}
	source, destination := dial(sender), dial(receiver)
	c := &UdpTransport{config: &UdpConfig{}, lifecycle: lifecycle{logger: silentLogger()}}
	var activity atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.udpCopy(source, destination, 0, false, time.Now(), &activity)
	}()
	b.Cleanup(func() { source.Close(); <-done })
	peer := source.LocalAddr().(*net.UDPAddr).AddrPort()
	body, reply := make([]byte, 1200), make([]byte, 65535)
	_ = receiver.SetReadDeadline(time.Now().Add(time.Hour))
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := sender.WriteToUDPAddrPort(body, peer); err != nil {
			b.Fatal(err)
		}
		n, err := receiver.Read(reply)
		if err != nil || n != len(body) {
			b.Fatalf("forwarded datagram: size=%d error=%v", n, err)
		}
	}
}
