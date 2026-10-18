package nativeudp

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type benchmarkSocket struct{ bytes atomic.Uint64 }

func (s *benchmarkSocket) WriteTo(p []byte, _ net.Addr) (int, error) {
	s.bytes.Add(uint64(len(p)))
	return len(p), nil
}
func (*benchmarkSocket) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, net.ErrClosed }
func (*benchmarkSocket) Close() error                           { return nil }
func (*benchmarkSocket) LocalAddr() net.Addr                    { return &net.UDPAddr{} }
func (*benchmarkSocket) SetDeadline(time.Time) error            { return nil }
func (*benchmarkSocket) SetReadDeadline(time.Time) error        { return nil }
func (*benchmarkSocket) SetWriteDeadline(time.Time) error       { return nil }
func benchmarkLink() *Link {
	return &Link{ctx: context.Background(), pc: &benchmarkSocket{}, peer: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, active: &keys{created: time.Now(), send: suite.Cipher([32]byte{1})}}
}

// Measures sender encryption/framing/fragmentation and shared-link contention.
// It excludes socket I/O, helper CPU and the remote peer; tunnelbench covers those.
func BenchmarkNativeUDPEncryptedSend(b *testing.B) {
	for _, size := range []int{64, 1000, 1200, 60000} {
		for _, parallel := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/parallel=%v", size, parallel), func(b *testing.B) {
				link := benchmarkLink()
				body := make([]byte, size)
				if err := link.Write(body); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				if parallel {
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if err := link.Write(body); err != nil {
								b.Error(err)
								return
							}
						}
					})
				} else {
					for b.Loop() {
						if err := link.Write(body); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
func TestNativeUDPSteadySendKeepsOnlyCipherNonceAllocations(t *testing.T) {
	link := benchmarkLink()
	body := make([]byte, 1200)
	for range 3 {
		if err := link.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	allocations := testing.AllocsPerRun(100, func() {
		if err := link.Write(body); err != nil {
			t.Fatal(err)
		}
	})
	if allocations > 2 {
		t.Fatalf("encrypted fragmented send exceeds its two cipher-nonce allocation budget: %.1f objects per packet", allocations)
	}
}
