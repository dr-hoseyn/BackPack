package transport

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// A quiet direction's first timer may fire while the opposite direction is
// still active. It must rearm to the shared last-activity deadline and then
// expire exactly there, without requiring a packet to wake the quiet copy.
func TestQuietUDPDirectionExpiresAtTheSharedActivityDeadline(t *testing.T) {
	for _, direction := range []string{"local-to-tunnel", "tunnel-to-local"} {
		t.Run(direction, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				s := &UdpTransport{config: &UdpConfig{}, lifecycle: lifecycle{logger: quietLogger()}}
				g := &udpGen{ctx: ctx}
				peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
				// No payload/socket I/O in the fake clock: the opposite copy is
				// represented by its shared activity update below.
				local := &LocalUDPConn{addr: peer, payload: make(chan []byte)}
				tunnel := &TunnelUDPConn{addr: peer, payload: make(chan []byte)}
				started := time.Now()
				var activity atomic.Int64
				done := make(chan time.Duration, 1)
				go func() {
					if direction == "local-to-tunnel" {
						s.udpLocalCopy(g, local, tunnel, started, &activity)
					} else {
						s.udpTunnelCopy(g, tunnel, local, started, &activity)
					}
					done <- time.Since(started)
				}()
				synctest.Wait()
				time.Sleep(50 * time.Second)
				touchUDPActivity(started, &activity)
				time.Sleep(11 * time.Second)
				select {
				case <-done:
					t.Fatal("quiet direction expired despite opposite traffic")
				default:
				}
				time.Sleep(48 * time.Second)
				select {
				case <-done:
					t.Fatal("quiet direction expired before the full shared idle budget")
				default:
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				select {
				case ended := <-done:
					if ended != 50*time.Second+idleForward {
						t.Fatalf("idle direction stopped after %v", ended)
					}
				default:
					t.Fatal("idle direction kept its flow alive past the shared deadline")
				}
			})
		})
	}
}
