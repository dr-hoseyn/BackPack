package transport

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestPendingClaimsStopWithTheirGeneration(t *testing.T) {
	for _, tc := range []struct {
		name  string
		judge func(context.Context, net.Conn)
	}{
		{"tcp", func(ctx context.Context, conn net.Conn) {
			server := &TcpTransport{config: &TcpConfig{Token: "claim-cancellation"}, lifecycle: lifecycle{logger: quietLogger()}}
			server.admitTunnelConn(&tcpGen{ctx: ctx}, conn)
		}},
		{"tcpmux", func(ctx context.Context, conn net.Conn) {
			server := &TcpMuxTransport{config: &TcpMuxConfig{Token: "claim-cancellation"}, lifecycle: lifecycle{logger: quietLogger()}}
			server.admitTunnelConn(&tcpMuxGen{ctx: ctx}, conn)
		}},
		{"udp", func(ctx context.Context, conn net.Conn) {
			server := &UdpTransport{config: &UdpConfig{Token: "claim-cancellation"}, lifecycle: lifecycle{logger: quietLogger()}}
			if server.validControlClaim(&udpGen{ctx: ctx}, conn) {
				t.Error("incomplete claim was accepted")
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { tc.judge(ctx, server); close(done) }()
			client.SetWriteDeadline(time.Now().Add(time.Second))
			// This write completes only after the judge started reading its
			// three-byte frame header. Leave the remaining bytes unsent.
			if _, err := client.Write([]byte{0}); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancelled generation retained a pending claim")
			}
			client.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := client.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("claim socket was not closed: %v", err)
			}
		})
	}
}
