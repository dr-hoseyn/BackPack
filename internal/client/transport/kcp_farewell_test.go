package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils"
	"github.com/xtaci/kcp-go/v5"
)

// KCP queues encrypted packets on a separate sender. Delay that sender so a
// successful control write cannot accidentally be mistaken for delivery.
type delayedKCPPacketConn struct{ net.PacketConn }

func (c delayedKCPPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	time.Sleep(50 * time.Millisecond)
	return c.PacketConn.WriteTo(p, addr)
}

func TestKCPClientGoodbyeReachesServerBeforeSocketCloses(t *testing.T) {
	listener, err := kcp.ListenWithOptions("127.0.0.1:0", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	session, err := kcp.NewConn4(1, listener.Addr(), nil, 0, 0, true, delayedKCPPacketConn{packet})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.SetNoDelay(1, 10, 2, 1)
	session.SetWriteDelay(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewKcpClient(ctx, &KcpConfig{KeepAlive: time.Minute}, silentLogger())
	if !client.state.SetConn(session) {
		t.Fatal("could not install the control session")
	}
	loop := client.control()
	done := make(chan struct{})
	go func() { defer close(done); loop.run() }()
	cancel()

	_ = listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	peer, err := listener.AcceptKCP()
	if err != nil {
		t.Fatalf("server never received the queued goodbye: %v", err)
	}
	defer peer.Close()
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := utils.ReceiveBinaryByte(peer)
	if err != nil || got != utils.SG_Closed {
		t.Fatalf("goodbye = %d, %v; want %d", got, err, utils.SG_Closed)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("client did not close its control session after sending the goodbye")
	}
	if _, err := session.Write([]byte("closed")); err == nil {
		t.Fatal("client left the control session open")
	}
}

func TestKCPClientGoodbyeCannotStallShutdown(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewKcpClient(ctx, &KcpConfig{KeepAlive: time.Minute}, silentLogger())
	client.state.SetConn(conn)
	loop := client.control()
	done := make(chan struct{})
	go func() { defer close(done); loop.run() }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked goodbye kept the client alive")
	}
}
