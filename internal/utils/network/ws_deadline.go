package network

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Gorilla resets the socket deadline before each frame. Preserve the stream's
// independently set deadline, including changes that interrupt a blocked write.
// No concurrent access to Gorilla's writer/deadline fields is needed.
type streamDeadlineConn struct {
	net.Conn
	mu             sync.Mutex
	frameDeadline  time.Time
	streamDeadline time.Time
	appliedWrite   time.Time
	writeSet       bool
	closed         bool
}

func (c *streamDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.frameDeadline = t
	deadline := c.effectiveWriteDeadline()
	// Gorilla refreshes this before every frame. Skip the socket timer
	// update when its effective deadline is already set.
	if c.writeSet && c.appliedWrite.Equal(deadline) {
		return nil
	}
	return c.applyWriteDeadline(deadline)
}

func (c *streamDeadlineConn) setStreamWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.streamDeadline = t
	return c.applyWriteDeadline(c.effectiveWriteDeadline())
}

// Both the WebSocket frame and the mux stream can limit a blocked write.
// Updating one deadline must not extend a tighter deadline set by the other.
func (c *streamDeadlineConn) effectiveWriteDeadline() time.Time {
	deadline := c.frameDeadline
	if !c.streamDeadline.IsZero() && (deadline.IsZero() || c.streamDeadline.Before(deadline)) {
		deadline = c.streamDeadline
	}
	return deadline
}

func (c *streamDeadlineConn) applyWriteDeadline(deadline time.Time) error {
	err := c.Conn.SetWriteDeadline(deadline)
	c.appliedWrite, c.writeSet = deadline, err == nil
	return err
}

// Upgrade setup uses the combined deadline, which also updates the write timer.
func (c *streamDeadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	c.frameDeadline = t
	err := c.Conn.SetDeadline(t)
	c.appliedWrite, c.writeSet = t, err == nil
	if deadline := c.effectiveWriteDeadline(); err == nil && !deadline.Equal(t) {
		err = c.applyWriteDeadline(deadline)
	}
	return err
}

func (c *streamDeadlineConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.writeSet = false
	c.mu.Unlock()
	// A failed mux session must interrupt both directions immediately. TLS
	// Close can block trying to send close_notify on a congested socket.
	if tls, ok := c.Conn.(interface{ NetConn() net.Conn }); ok {
		rawErr := tls.NetConn().Close()
		err := c.Conn.Close()
		if rawErr == nil && errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		return rawErr
	}
	return c.Conn.Close()
}

func (c *streamDeadlineConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

type wsStreamResponse struct{ http.ResponseWriter }

func (w wsStreamResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w wsStreamResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &streamDeadlineConn{Conn: c}, rw, nil
}

// WSMuxResponse preserves net.Conn deadline semantics after the upgrade.
func WSMuxResponse(w http.ResponseWriter) http.ResponseWriter { return wsStreamResponse{w} }
