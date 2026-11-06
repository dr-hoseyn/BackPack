package network

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

// NewWatchedMuxSession ties smux's lifetime to terminal socket errors. smux
// reports those errors to stream operations but leaves CloseChan open until
// its keepalive closes the session. An idle pool worker waits on CloseChan.
func NewWatchedMuxSession(conn net.Conn, cfg *smux.Config, start func(io.ReadWriteCloser, *smux.Config) (*smux.Session, error)) (*smux.Session, error) {
	if cfg == nil {
		cfg = smux.DefaultConfig()
	}
	socket := &muxSocket{Conn: conn, timeout: cfg.KeepAliveTimeout}
	var wire net.Conn = socket
	if writer, ok := conn.(muxVectorWriter); ok {
		wire = &muxVectorSocket{muxSocket: socket, writer: writer}
	}
	session, err := start(wire, cfg)
	if err != nil {
		return nil, err
	}
	socket.mu.Lock()
	socket.session = session
	failed := socket.failed
	socket.mu.Unlock()
	if failed {
		_ = session.Close()
	}
	return session, nil
}

type muxSocket struct {
	net.Conn
	timeout  time.Duration
	deadline time.Time  // only smux's sender accesses this
	mu       sync.Mutex // used only during registration or after an I/O failure
	session  *smux.Session
	failed   bool
}

func (c *muxSocket) result(n int, err error) (int, error) {
	if err != nil {
		c.mu.Lock()
		first := !c.failed
		c.failed = true
		session := c.session
		c.mu.Unlock()
		if first && session != nil && !session.IsClosed() {
			// Close outside smux's sender/receiver to avoid re-entering it.
			go session.Close()
		}
	}
	return n, err
}

func (c *muxSocket) Read(p []byte) (int, error) {
	return c.result(c.Conn.Read(p))
}

func (c *muxSocket) prepareWrite() error {
	if c.timeout <= 0 {
		return nil
	}
	now := time.Now()
	// A stream deadline only bounds its wait for smux's sender. Bound the
	// physical write too, without resetting its timer for every mux frame.
	if c.deadline.Sub(now) <= c.timeout/2 {
		c.deadline = now.Add(c.timeout)
		return c.Conn.SetWriteDeadline(c.deadline)
	}
	return nil
}

func (c *muxSocket) Write(p []byte) (int, error) {
	if err := c.prepareWrite(); err != nil {
		return c.result(0, err)
	}
	n, err := c.Conn.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return c.result(n, err)
}

type muxVectorWriter interface {
	WriteBuffers([][]byte) (int, error)
}

type muxVectorSocket struct {
	*muxSocket
	writer muxVectorWriter
}

func (c *muxVectorSocket) WriteBuffers(p [][]byte) (int, error) {
	if err := c.prepareWrite(); err != nil {
		return c.result(0, err)
	}
	return c.result(c.writer.WriteBuffers(p))
}
