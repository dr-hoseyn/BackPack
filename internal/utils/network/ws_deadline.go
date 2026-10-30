package network

import (
	"bufio"
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
}

func (c *streamDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.writeDeadline.IsZero() && (t.IsZero() || c.writeDeadline.Before(t)) {
		t = c.writeDeadline
	}
	return c.Conn.SetWriteDeadline(t)
}

func (c *streamDeadlineConn) setStreamWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return c.Conn.SetWriteDeadline(t)
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
