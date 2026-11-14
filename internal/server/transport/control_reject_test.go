package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// The same admission race affects TCP control channels used by the other
// carriers. The candidate remains the caller's responsibility when the seat
// refuses an ended generation; no control loop will be started to close it.
func TestRejectedControlCandidatesAreClosed(t *testing.T) {
	for name, seat := range map[string]func(context.Context, net.Conn){
		"tcp": func(ctx context.Context, conn net.Conn) {
			(&TcpTransport{}).seatClient(&tcpGen{ctx: ctx}, controlCandidate{conn: conn})
		},
		"tcpmux": func(ctx context.Context, conn net.Conn) {
			(&TcpMuxTransport{}).seatClient(&tcpMuxGen{ctx: ctx}, controlCandidate{conn: conn})
		},
		"kcp": func(ctx context.Context, conn net.Conn) {
			(&KcpTransport{}).seatClient(&kcpGen{ctx: ctx}, conn)
		},
		"udp": func(ctx context.Context, conn net.Conn) {
			(&UdpTransport{}).seatClient(&udpGen{ctx: ctx}, conn)
		},
		"quic": func(ctx context.Context, conn net.Conn) {
			(&QuicTransport{}).seatClient(&quicGen{ctx: ctx}, quicClaim{ctrl: conn})
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			candidate, peer := net.Pipe()
			defer candidate.Close()
			defer peer.Close()
			seat(ctx, candidate)
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			var p [1]byte
			if n, err := peer.Read(p[:]); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("rejected candidate was not closed: %d, %v", n, err)
			}
		})
	}
}
