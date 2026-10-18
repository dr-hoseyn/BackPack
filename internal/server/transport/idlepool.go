package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
)

// Adaptive clients retire unused connections without assigning a destination.
// Observe EOF while queued so the server cannot pair a user with a stale slot.
// The one-byte lookahead is handed to the eventual reader; it never consumes
// application data invisibly. Claim and idle EOF compete atomically. Once
// claimed, directional EOF must leave the response direction intact.
type idlePoolConn struct {
	net.Conn
	state  atomic.Uint32 // 0 queued, 1 claimed, 2 closed while queued
	done   chan struct{}
	readMu sync.Mutex
	first  bool
	byte   [1]byte
	n      int
	err    error
	stop   func() bool
}

func watchIdlePool(ctx context.Context, conn net.Conn) *idlePoolConn {
	c := &idlePoolConn{Conn: conn, done: make(chan struct{}), first: true}
	c.stop = context.AfterFunc(ctx, func() { conn.Close() })
	go func() {
		c.n, c.err = conn.Read(c.byte[:])
		if c.err != nil && c.n == 0 && c.state.CompareAndSwap(0, 2) {
			conn.Close()
		}
		close(c.done)
	}()
	return c
}
func (c *idlePoolConn) claim() bool { return c.state.CompareAndSwap(0, 1) }
func (c *idlePoolConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.first {
		<-c.done
		c.first = false
		if c.n != 0 {
			p[0] = c.byte[0]
			return 1, nil
		}
	}
	if c.err != nil {
		return 0, c.err
	}
	return c.Conn.Read(p)
}
func (c *idlePoolConn) Close() error { err := c.Conn.Close(); <-c.done; c.stop(); return err }
func (c *idlePoolConn) CloseWrite() error {
	if v, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return v.CloseWrite()
	}
	return c.Conn.Close()
}
