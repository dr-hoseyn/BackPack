package transport

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type congestedForwardSocket struct {
	started   chan struct{}
	closed    chan struct{}
	start     sync.Once
	closeOnce sync.Once
}

func (c *congestedForwardSocket) ReadFromUDP([]byte) (int, *net.UDPAddr, error) {
	<-c.closed
	return 0, nil, net.ErrClosed
}
func (c *congestedForwardSocket) ReadFromUDPAddrPort([]byte) (int, netip.AddrPort, error) {
	<-c.closed
	return 0, netip.AddrPort{}, net.ErrClosed
}
func (c *congestedForwardSocket) WriteToUDP([]byte, *net.UDPAddr) (int, error) {
	c.start.Do(func() { close(c.started) })
	<-c.closed
	return 0, net.ErrClosed
}
func (c *congestedForwardSocket) WriteToUDPAddrPort(b []byte, _ netip.AddrPort) (int, error) {
	return c.WriteToUDP(b, nil)
}
func (c *congestedForwardSocket) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9000}
}
func (c *congestedForwardSocket) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// A reply can wait for UDP send-buffer space while holding its flow's write
// lock. Closing the listener must interrupt that I/O before waiting for flows.
func TestClosingUDPForwarderInterruptsCongestedReplies(t *testing.T) {
	socket := &congestedForwardSocket{started: make(chan struct{}), closed: make(chan struct{})}
	fw := &udpForwarder{conn: socket, logger: quietLogger(), flows: map[string]*udpFlow{}}
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7000}
	flow := newUDPFlow(fw, peer, peer.String())
	fw.flows[flow.key] = flow
	writeDone := make(chan error, 1)
	go func() { _, err := flow.Write([]byte{0, 1, 'x'}); writeDone <- err }()
	select {
	case <-socket.started:
	case <-time.After(time.Second):
		socket.Close()
		t.Fatal("reply never reached the socket")
	}
	closed := make(chan struct{})
	go func() { fw.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		socket.Close()
		<-closed
		<-writeDone
		t.Fatal("closing the UDP listener waited for the blocked reply instead of interrupting it")
	}
	if err := <-writeDone; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("blocked reply ended with %v", err)
	}
}

// Include the real socket receive, source lookup, queued frame and consumer.
// A mapping that already exists should not allocate an address per datagram.
func BenchmarkForwardedUDPDelivery(b *testing.B) {
	fw, err := newUDPForwarder(quietLogger(), "127.0.0.1:0", "127.0.0.1:9000")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(fw.close)
	admitted := make(chan net.Conn, 1)
	go fw.run(func(c net.Conn, _ string) bool { admitted <- c; return true })
	peer, err := net.Dial("udp", fw.conn.LocalAddr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()
	payload := bytes.Repeat([]byte{0x5a}, 1200)
	if _, err = peer.Write(payload); err != nil {
		b.Fatal(err)
	}
	var flow net.Conn
	select {
	case flow = <-admitted:
	case <-time.After(time.Second):
		b.Fatal("no flow")
	}
	defer flow.Close()
	got := make([]byte, len(payload)+2)
	if _, err = flow.Read(got); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err = peer.Write(payload); err != nil {
			b.Fatal(err)
		}
		n, err := flow.Read(got)
		if err != nil || n != len(got) || !bytes.Equal(got[2:], payload) {
			b.Fatalf("corrupt or missing datagram: n=%d err=%v", n, err)
		}
	}
	b.StopTimer()
}
