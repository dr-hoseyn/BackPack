package transport

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils/network"
)

// Exercise both address families and IPv4 arriving at a dual-stack listener.
// Repeated packets must keep their own source flow, including empty and large
// datagrams, and replies must still originate at the forwarded port.
func TestForwardedUDPValueAddressesPreserveDatagramsAndSourceIsolation(t *testing.T) {
	for _, tc := range []struct{ name, bind, dial string }{
		{"IPv4", "127.0.0.1:0", "udp4"},
		{"IPv6", "[::1]:0", "udp6"},
		{"dual-stack-IPv4", "[::]:0", "udp4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw, err := newUDPForwarder(quietLogger(), tc.bind, "9000")
			if err != nil {
				if tc.dial == "udp6" || tc.bind == "[::]:0" {
					t.Skipf("IPv6 listener unavailable: %v", err)
				}
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			stop := context.AfterFunc(ctx, fw.close)
			defer stop()
			admitted := make(chan net.Conn, 4)
			done := make(chan struct{})
			go func() {
				defer close(done)
				fw.run(func(c net.Conn, _ string) bool {
					select {
					case admitted <- c:
						return true
					default:
						return false
					}
				})
			}()
			defer func() { fw.close(); <-done }()
			bound := fw.conn.LocalAddr().(*net.UDPAddr)
			address := bound.String()
			if tc.bind == "[::]:0" {
				address = fmt.Sprintf("127.0.0.1:%d", bound.Port)
			}
			peers := make([]net.Conn, 2)
			flows := make([]net.Conn, 2)
			for i := range peers {
				peer, err := net.Dial(tc.dial, address)
				if err != nil {
					t.Fatal(err)
				}
				defer peer.Close()
				_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
				peers[i] = peer
			}
			buf := make([]byte, network.MaxDatagram)
			for _, size := range []int{0, 64, 1200, 60000} {
				for i, peer := range peers {
					payload := bytes.Repeat([]byte{byte(i + 1)}, size)
					if _, err := peer.Write(payload); err != nil {
						t.Fatal(err)
					}
					if flows[i] == nil {
						select {
						case flows[i] = <-admitted:
						case <-ctx.Done():
							t.Fatal("opening datagram did not create a flow")
						}
						from := flows[i].RemoteAddr().(*net.UDPAddr)
						want := peer.LocalAddr().(*net.UDPAddr)
						if from.Port != want.Port || !from.IP.Equal(want.IP) || from.Zone != want.Zone {
							t.Fatalf("source address changed: got %v, want %v", from, want)
						}
					}
					n, err := network.ReadDatagram(flows[i], buf)
					if err != nil || !bytes.Equal(buf[:n], payload) {
						t.Fatalf("source %d, size %d: datagram changed, size=%d error=%v", i, size, n, err)
					}
					if err := network.WriteDatagram(flows[i], payload); err != nil {
						t.Fatal(err)
					}
					n, err = peer.Read(buf)
					if err != nil || !bytes.Equal(buf[:n], payload) {
						t.Fatalf("source %d, size %d: reply changed or reached another source, size=%d error=%v", i, size, n, err)
					}
				}
			}
			select {
			case <-admitted:
				t.Fatal("repeated source datagrams created a new flow")
			default:
			}
		})
	}
}

func TestClosingAnOldUDPFlowPreservesItsReplacement(t *testing.T) {
	fw := testForwarder(t, "9000")
	peer := netip.MustParseAddrPort("127.0.0.1:5000")
	old, replacement := newUDPFlow(fw, peer), newUDPFlow(fw, peer)
	fw.flows[peer] = replacement
	old.Close()
	if fw.flows[peer] != replacement {
		t.Fatal("closing the old source flow removed its replacement")
	}
	replacement.Close()
	if len(fw.flows) != 0 {
		t.Fatal("closed replacement left its source mapping behind")
	}
}
