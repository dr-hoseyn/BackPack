package nativeudp

import (
	"bytes"
	"fmt"
	"net"
	"runtime"
	"testing"
	"time"
)

func entrySocket(t testing.TB, network, host string) *net.UDPConn {
	t.Helper()
	addr, err := net.ResolveUDPAddr(network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUDP(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Exercise real encrypted request/reply paths. One sender reaches two forwarded
// ports: sharing a peer address must never merge their backend selections.
func TestNativeUDPSourceFlowsStaySeparateAcrossForwardedPorts(t *testing.T) {
	for _, tc := range []struct{ network, host string }{{"udp4", "127.0.0.1"}, {"udp6", "::1"}} {
		t.Run(tc.network, func(t *testing.T) {
			transport := entrySocket(t, tc.network, tc.host)
			clientTransport := entrySocket(t, tc.network, tc.host)
			const token = "native-entry-test-private-token"
			server, err := newEngine(t.Context(), transport, nil, token, 2, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close() })
			client, err := StartClient(t.Context(), clientTransport, transport.LocalAddr(), token, 2, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			entries := []*net.UDPConn{entrySocket(t, tc.network, tc.host), entrySocket(t, tc.network, tc.host)}
			for i, entry := range entries {
				backend := entrySocket(t, tc.network, tc.host)
				marker := byte(i + 1)
				go func() {
					buf := make([]byte, 65535)
					for {
						n, from, err := backend.ReadFromUDPAddrPort(buf[1:])
						if err != nil {
							return
						}
						buf[0] = marker
						if _, err := backend.WriteToUDPAddrPort(buf[:n+1], from); err != nil {
							return
						}
					}
				}()
				server.sockets = append(server.sockets, entry)
				server.wg.Add(1)
				go server.entry(entry, []string{backend.LocalAddr().String()})
			}
			server.start()
			deadline := time.Now().Add(5 * time.Second)
			for !client.Stats().Connected || !server.Stats().Connected {
				if time.Now().After(deadline) {
					t.Fatal("native UDP handshake did not complete")
				}
				time.Sleep(time.Millisecond)
			}
			sender := entrySocket(t, tc.network, tc.host)
			buf := make([]byte, maxPayload)
			for round := range 2 {
				for i, entry := range entries {
					for _, size := range []int{0, 64, 1200, 8000} {
						payload := bytes.Repeat([]byte{byte(round + size)}, size)
						if _, err := sender.WriteToUDP(payload, entry.LocalAddr().(*net.UDPAddr)); err != nil {
							t.Fatal(err)
						}
						_ = sender.SetReadDeadline(time.Now().Add(3 * time.Second))
						n, from, err := sender.ReadFromUDP(buf)
						if err != nil {
							t.Fatal(err)
						}
						if from.String() != entry.LocalAddr().String() || n != size+1 || buf[0] != byte(i+1) || !bytes.Equal(buf[1:n], payload) {
							t.Fatalf("port %d round %d size %d: wrong source, backend or payload", i, round, size)
						}
					}
				}
			}
			if got := server.Stats().Flows; got != 2 {
				t.Fatalf("one source on two forwarded ports created %d flows, want 2", got)
			}
		})
	}
}

// forwardedEntrySink keeps the actual source socket, lookup and encryption path,
// with a synchronous sink instead of helper processes or a remote network.
func forwardedEntrySink(t testing.TB, network, host string, size int) func() {
	t.Helper()
	entry := entrySocket(t, network, host)
	pc := entrySocket(t, network, host)
	e, err := newEngine(t.Context(), pc, nil, "native-entry-benchmark-private-token", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	e.link.Close()
	e.link = benchmarkLink()
	// No background maintenance in this measurement; keep the sink connected
	// even when the caller chooses a long benchmark duration.
	e.link.lastRx = time.Now().Add(24 * time.Hour)
	e.link.active.created = e.link.lastRx
	e.wg.Add(1)
	go e.entry(entry, []string{"127.0.0.1:53"})
	t.Cleanup(func() { entry.Close(); e.wg.Wait(); e.cancel() })
	sender, err := net.DialUDP(network, nil, entry.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sender.Close() })
	body := make([]byte, size)
	var sent uint64
	return func() {
		if _, err := sender.Write(body); err != nil {
			t.Fatal(err)
		}
		sent++
		for e.link.sent.Load() < sent {
			if e.drops.Load() != 0 || t.Context().Err() != nil {
				t.Fatal("entry dropped a packet or stopped before the sink received it")
			}
			runtime.Gosched()
		}
	}
}

func TestNativeUDPForwardedEntryKeepsOnlyCipherNonceAllocations(t *testing.T) {
	for _, tc := range []struct{ network, host string }{{"udp4", "127.0.0.1"}, {"udp6", "::1"}} {
		for _, size := range []int{64, 1200} {
			t.Run(fmt.Sprintf("%s/bytes=%d", tc.network, size), func(t *testing.T) {
				send := forwardedEntrySink(t, tc.network, tc.host, size)
				send()
				// One primitive-cipher nonce per fragment, with the same target
				// framing overhead as the engine's write method.
				fragments := (size + 10 + len("127.0.0.1:53") + fragmentSize - 1) / fragmentSize
				if got := testing.AllocsPerRun(100, send); got > float64(fragments) {
					t.Fatalf("forwarded entry allocated %.1f objects per datagram, want at most %d cipher nonces", got, fragments)
				}
			})
		}
	}
}

// Includes real entry socket reads, flow lookup, framing and encryption. The
// encrypted destination is a sink, excluding remote helpers and WAN variance.
func BenchmarkNativeUDPForwardedEntry(b *testing.B) {
	for _, tc := range []struct{ network, host string }{{"udp4", "127.0.0.1"}, {"udp6", "::1"}} {
		for _, size := range []int{64, 1200} {
			b.Run(fmt.Sprintf("%s/bytes=%d", tc.network, size), func(b *testing.B) {
				send := forwardedEntrySink(b, tc.network, tc.host, size)
				send()
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					send()
				}
			})
		}
	}
}
