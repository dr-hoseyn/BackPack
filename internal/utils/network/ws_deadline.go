package network

import (
	"bufio"
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
}

func (c *streamDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frameDeadline = t
	return c.applyWriteDeadline()
}

func (c *streamDeadlineConn) setStreamWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streamDeadline = t
	return c.applyWriteDeadline()
}

// Both the WebSocket frame and the mux stream can limit a blocked write.
// Updating one deadline must not extend a tighter deadline set by the other.
func (c *streamDeadlineConn) applyWriteDeadline() error {
	deadline := c.frameDeadline
	if !c.streamDeadline.IsZero() && (deadline.IsZero() || c.streamDeadline.Before(deadline)) {
		deadline = c.streamDeadline
	}
	return c.Conn.SetWriteDeadline(deadline)
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
