package network

import (
	"net"
	"os"
	"sync"
	"time"
)

// A CONNECT parser can read the peer's first bytes with the HTTP headers.
// Keep only that prefix; ordinary reads then go straight to the socket.
type proxyPrefixConn struct {
	net.Conn
	mu           sync.Mutex
	prefix       []byte
	readDeadline time.Time
	closed       bool
}

func preserveProxyPrefix(conn net.Conn, prefix []byte) net.Conn {
	c := &proxyPrefixConn{Conn: conn, prefix: append([]byte(nil), prefix...)}
	if tcp, ok := conn.(*net.TCPConn); ok {
		return &proxyPrefixTCPConn{proxyPrefixConn: c, tcp: tcp}
	}
	return c
}

func (c *proxyPrefixConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}
	if len(c.prefix) != 0 {
		if !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline) {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		if len(c.prefix) == 0 {
			c.prefix = nil
		}
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}

func (c *proxyPrefixConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if err := c.Conn.SetReadDeadline(t); err != nil {
		return err
	}
	c.readDeadline = t
	return nil
}

func (c *proxyPrefixConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	if err := c.Conn.SetDeadline(t); err != nil {
		return err
	}
	c.readDeadline = t
	return nil
}

func (c *proxyPrefixConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.prefix = nil
	c.mu.Unlock()
	return c.Conn.Close()
}

// Expose directional EOF only when the underlying connection supports it.
// Deliberately omit UnderlyingConn: a splice path must not bypass the prefix.
type proxyPrefixTCPConn struct {
	*proxyPrefixConn
	tcp *net.TCPConn
}

func (c *proxyPrefixTCPConn) CloseWrite() error      { return c.tcp.CloseWrite() }
func (*proxyPrefixTCPConn) PreservesDirectionalEOF() {}
