package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestPacedUDPPacketDoesNotBlockAssociationRetirementOrReachItsReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	old := &TunnelUDPConn{addr: peer, payload: make(chan []byte, 1)}
	replacement := &TunnelUDPConn{addr: peer, payload: make(chan []byte, 1)}
	key := peer.String()
	bucket := rate.NewLimiter(1, 1)
	bucket.Allow() // the incoming byte must wait about a second
	s := &UdpTransport{lifecycle: lifecycle{logger: quietLogger()},
		limits: &limiter{bucket: bucket}, activeConnections: map[string]*TunnelUDPConn{key: old}}
	done := make(chan struct{})
	go func() { defer close(done); s.deliverTunnelPayload(ctx, key, []byte{1}) }()
	deadline := time.Now().Add(time.Second)
	for bucket.Tokens() > -0.5 {
		if time.Now().After(deadline) {
			t.Fatal("packet never entered pacing wait")
		}
		time.Sleep(time.Millisecond)
	}
	retired := make(chan struct{})
	go func() {
		s.dropTunnelConn(old)
		s.activeMu.Lock()
		s.activeConnections[key] = replacement
		s.activeMu.Unlock()
		close(retired)
	}()
	select {
	case <-retired:
	case <-time.After(200 * time.Millisecond):
		cancel()
		<-done
		<-retired
		t.Fatal("bandwidth pacing blocked association retirement")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pacing did not finish")
	}
	if len(replacement.payload) != 0 {
		t.Fatal("a paced packet from the old association reached its replacement")
	}
}
