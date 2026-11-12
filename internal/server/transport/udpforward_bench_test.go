package transport

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils/network"
)

// Measure the actual socket reader, source lookup and framed queue together.
// An established flow should not allocate an address or format it per packet.
func BenchmarkForwardedUDPReceive(b *testing.B) {
	for _, size := range []int{0, 64, 1200, 60000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			fw, err := newUDPForwarder(quietLogger(), "127.0.0.1:0", "9000")
			if err != nil {
				b.Fatal(err)
			}
			admitted := make(chan net.Conn, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				fw.run(func(c net.Conn, _ string) bool { admitted <- c; return true })
			}()
			// udpFlow intentionally has no read deadline. Bound a broken
			// forwarding benchmark too, so its cleanup can always join run.
			timer := time.AfterFunc(time.Minute, fw.close)
			b.Cleanup(func() { timer.Stop(); fw.close(); <-done })
			peer, err := net.Dial("udp4", fw.conn.LocalAddr().String())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { peer.Close() })
			_ = peer.SetWriteDeadline(time.Now().Add(time.Minute))
			if _, err := peer.Write([]byte{1}); err != nil {
				b.Fatal(err)
			}
			var flow net.Conn
			select {
			case flow = <-admitted:
			case <-time.After(5 * time.Second):
				b.Fatal("opening datagram was not admitted")
			}
			buf := make([]byte, network.MaxDatagram)
			if _, err := network.ReadDatagram(flow, buf); err != nil {
				b.Fatal(err)
			}
			payload := bytes.Repeat([]byte{0x73}, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := peer.Write(payload); err != nil {
					b.Fatal(err)
				}
				n, err := network.ReadDatagram(flow, buf)
				if err != nil || n != size {
					b.Fatalf("forwarded size=%d error=%v, want %d bytes", n, err, size)
				}
			}
			b.StopTimer()
			if !bytes.Equal(buf[:size], payload) {
				b.Fatal("forwarded payload changed")
			}
		})
	}
}
