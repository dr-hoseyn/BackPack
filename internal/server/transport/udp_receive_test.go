package transport

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func rawTunnelReceiver(t testing.TB, family string) (*net.UDPConn, <-chan []byte) {
	t.Helper()
	network, host := "udp4", "127.0.0.1"
	if family == "udp6" {
		network, host = "udp6", "::1"
	} else if family == "dual-stack" {
		network, host = "udp", "::"
	}
	addr, err := net.ResolveUDPAddr(network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUDP(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	peer := listener.LocalAddr().(*net.UDPAddr)
	if family == "dual-stack" {
		network = "udp4"
		peer = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: peer.Port}
	}
	sender, err := net.DialUDP(network, nil, peer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sender.Close() })
	payload := make(chan []byte, 1)
	s := &UdpTransport{
		config: &UdpConfig{}, lifecycle: lifecycle{logger: quietLogger()},
		activeConnections: map[string]*TunnelUDPConn{
			sender.LocalAddr().String(): {addr: sender.LocalAddr().(*net.UDPAddr), payload: payload},
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.acceptTunnelConn(&udpGen{ctx: ctx}, listener)
	}()
	t.Cleanup(func() { cancel(); listener.Close(); <-done })
	return sender, payload
}

func TestRawUDPTunnelValueReadsFindExistingIPv4AndIPv6Associations(t *testing.T) {
	for _, family := range []string{"udp4", "udp6", "dual-stack"} {
		t.Run(family, func(t *testing.T) {
			sender, payloads := rawTunnelReceiver(t, family)
			for _, size := range []int{64, 0, 1200, 60000, 7} {
				body := bytes.Repeat([]byte{byte(size)}, size)
				if _, err := sender.Write(body); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-payloads:
					if !bytes.Equal(got, body) {
						t.Fatalf("%d-byte datagram changed during association lookup", size)
					}
				case <-time.After(time.Second):
					t.Fatalf("%d-byte datagram missed its existing association", size)
				}
			}
		})
	}
}

func BenchmarkRawUDPTunnelReceive(b *testing.B) {
	sender, payloads := rawTunnelReceiver(b, "udp4")
	body := make([]byte, 1200)
	send := func() {
		if _, err := sender.Write(body); err != nil {
			b.Fatal(err)
		}
		if got := <-payloads; len(got) != len(body) {
			b.Fatalf("received %d bytes, want %d", len(got), len(body))
		}
	}
	send()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		send()
	}
}
