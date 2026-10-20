package network

import (
	"bufio"
	"net"
)

// Mux transports use HTTP Upgrade followed by the existing raw smux wire
// format. Gorilla's response reader may prefetch the first smux frames along
// with HTTP 101; NetConn then bypasses that reader and loses those frames.
// Bound the reader's input at the HTTP header and keep any prefetched payload
// on the connection that NetConn exposes. Framed websocket/control paths must
// retain Gorilla's normal reader and never use this adapter.
type muxUpgradeConn struct {
	net.Conn
	reader *bufio.Reader
	tail   uint32
	done   bool
}

func muxUpgradeStream(conn net.Conn) net.Conn {
	return &muxUpgradeConn{Conn: conn, reader: bufio.NewReader(conn)}
}

func (c *muxUpgradeConn) Read(p []byte) (int, error) {
	if c.done {
		if c.reader.Buffered() != 0 {
			return c.reader.Read(p)
		}
		// No extra buffering or copies on the steady-state data path.
		return c.Conn.Read(p)
	}
	for i := range p {
		b, err := c.reader.ReadByte()
		if err != nil {
			return i, err
		}
		p[i] = b
		c.tail = c.tail<<8 | uint32(b)
		if c.tail == 0x0d0a0d0a { // end of the HTTP response headers
			c.done = true
			return i + 1, nil
		}
	}
	return len(p), nil
}
