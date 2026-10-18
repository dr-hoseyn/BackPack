package adaptive

import (
	"net"
	"sync/atomic"
)

// Counts actual stream bytes including partial/failed probes and duplex framing.
// TLS/QUIC framing and native datagram verification are outside these counters.
type probeCountingConn struct {
	net.Conn
	sent, received *atomic.Uint64
}

func (c *probeCountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.sent.Add(uint64(n))
	return n, err
}
func (c *probeCountingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.received.Add(uint64(n))
	return n, err
}
