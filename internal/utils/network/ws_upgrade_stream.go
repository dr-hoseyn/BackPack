package network

import (
	"bufio"
	"net"
	"sync"
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

var muxUpgradeReaders = sync.Pool{New: func() any { return bufio.NewReader(nil) }}

func muxUpgradeStream(conn net.Conn) net.Conn {
	reader := muxUpgradeReaders.Get().(*bufio.Reader)
	reader.Reset(conn)
	return &muxUpgradeConn{Conn: &streamDeadlineConn{Conn: conn}, reader: reader}
}

func (c *muxUpgradeConn) setStreamWriteDeadline(t time.Time) error {
	return c.Conn.(*streamDeadlineConn).setStreamWriteDeadline(t)
}

func (c *muxUpgradeConn) Read(p []byte) (int, error) {
	if c.reader == nil {
		// No extra buffering or copies on the steady-state data path.
		return c.Conn.Read(p)
	}
	if c.done {
		n, err := c.reader.Read(p)
		if c.reader.Buffered() == 0 {
			c.releaseReader()
		}
		return n, err
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
			if c.reader.Buffered() == 0 {
				c.releaseReader()
			}
			return i + 1, nil
		}
	}
	return len(p), nil
}

func (c *muxUpgradeConn) releaseReader() {
	c.reader.Reset(nil)
	muxUpgradeReaders.Put(c.reader)
	c.reader = nil
}

// WSMuxUpgradeStream bounds the HTTP reader and preserves stream deadlines.
func WSMuxUpgradeStream(conn net.Conn) net.Conn { return muxUpgradeStream(conn) }
