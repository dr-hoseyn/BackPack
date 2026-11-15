package transport

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/testport"
)

func TestForwarderShutdownReleasesTCPAndUDPTogether(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := fmt.Sprintf("127.0.0.1:%d", testport.Free(t))
	var listeners listenerSet
	queue := make(chan LocalTCPConn, 1)
	forwarder := portForwarder{ctx: ctx, acceptUDP: true, queue: queue, listeners: &listeners, log: quietLogger()}
	done := make(chan struct{})
	go func() { forwarder.listen(address, "9000"); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("forwarder did not stop")
		}
	})
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	target, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	ready := false
	deadline := time.After(2 * time.Second)
	for !ready {
		select {
		case <-ticker.C:
			_, _ = peer.WriteToUDP([]byte("ready"), target)
		case local := <-queue:
			local.conn.Close()
			ready = true
		case <-deadline:
			t.Fatal("UDP sibling did not carry a real datagram")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not join UDP shutdown")
	}
	listeners.wait(context.Background())
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("TCP port still held after shutdown: %v", err)
	}
	tcp.Close()
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatalf("UDP port still held after shutdown: %v", err)
	}
	udp.Close()
}
