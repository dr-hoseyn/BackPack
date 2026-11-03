package network

import (
	"bufio"
	"errors"
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
	mu            sync.Mutex
	writeDeadline time.Time
	appliedWrite  time.Time
	writeSet      bool
	closed        bool
}

func (c *streamDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.writeDeadline.IsZero() && (t.IsZero() || c.writeDeadline.Before(t)) {
		t = c.writeDeadline
	}
	if c.closed {
		return net.ErrClosed
	}
	// Gorilla calls this before every frame, almost always with the same zero
	// deadline. Avoid taking the underlying socket's netpoll/timer locks again
	// when its effective deadline has not changed.
	if c.writeSet && c.appliedWrite.Equal(t) {
		return nil
	}
	return c.applyWriteDeadline(t)
}

func (c *streamDeadlineConn) setStreamWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	// Explicit stream changes must always reach the socket, including changes
	// that interrupt a blocked writer. The cache only suppresses frame refreshes.
	return c.applyWriteDeadline(t)
}

// applyWriteDeadline is called with mu held. Failed changes are never cached.
func (c *streamDeadlineConn) applyWriteDeadline(t time.Time) error {
	err := c.Conn.SetWriteDeadline(t)
	c.writeSet = err == nil
	c.appliedWrite = t
	return err
}

// Upgrade setup uses the combined deadline. Track it as well, so clearing the
// handshake budget cannot leave a stale cached write deadline after hijacking.
func (c *streamDeadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.Conn.SetDeadline(t)
	c.writeSet = err == nil
	c.appliedWrite = t
	return err
}

func (c *streamDeadlineConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.writeSet = false
	c.mu.Unlock()
	// A failed mux session must interrupt both directions immediately. TLS
	// Close may otherwise wait five seconds trying to send close_notify into
	// the same congested socket whose framed write just timed out.
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
