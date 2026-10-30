package network

import (
	"bufio"
	"net"
	"time"
)

// Mux transports negotiate framed or legacy raw smux after HTTP Upgrade.
// Gorilla's response reader may prefetch the first payload along with HTTP
// 101; a legacy NetConn reader would bypass and lose those bytes.
// Bound the reader's input at the HTTP header and keep any prefetched payload
// on the underlying connection. A framed peer then reads it through Gorilla;
// a legacy peer reads it directly. No mode is chosen before the response.
type muxUpgradeConn struct {
	net.Conn
	reader *bufio.Reader
	tail   uint32
	done   bool
}

func muxUpgradeStream(conn net.Conn) net.Conn {
	return &muxUpgradeConn{Conn: &streamDeadlineConn{Conn: conn}, reader: bufio.NewReader(conn)}
}

func (c *muxUpgradeConn) setStreamWriteDeadline(t time.Time) error {
	return c.Conn.(*streamDeadlineConn).setStreamWriteDeadline(t)
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

// WSMuxUpgradeStream bounds the HTTP reader and preserves stream deadlines.
func WSMuxUpgradeStream(conn net.Conn) net.Conn { return muxUpgradeStream(conn) }
