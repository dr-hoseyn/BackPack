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
	} else if tcp, ok := conn.(*net.TCPConn); ok {
		wire = &muxTCPSocket{muxSocket: socket, tcp: tcp, buffers: make(net.Buffers, 0, 2)}
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
	return c.writeResult(n, len(p), err)
}

func (c *muxSocket) writeResult(n, want int, err error) (int, error) {
	if err == nil && n != want {
		err = io.ErrShortWrite
	}
	return c.result(n, err)
}

type muxVectorWriter interface {
	WriteBuffers([][]byte) (int, error)
}

func muxVectorBytes(parts [][]byte) int {
	n := 0
	for _, part := range parts {
		n += len(part)
	}
	return n
}

type muxVectorSocket struct {
	*muxSocket
	writer muxVectorWriter
}

func (c *muxVectorSocket) WriteBuffers(p [][]byte) (int, error) {
	if err := c.prepareWrite(); err != nil {
		return c.result(0, err)
	}
	want := muxVectorBytes(p)
	n, err := c.writer.WriteBuffers(p)
	return c.writeResult(n, want, err)
}

// Only a concrete TCP socket takes this path. Passing TLS or WebSocket
// wrappers to net.Buffers would turn one frame into separate writes instead
// of a single vectored socket write. smux has exactly one sender.
type muxTCPSocket struct {
	*muxSocket
	tcp     *net.TCPConn
	buffers net.Buffers
}

func (c *muxTCPSocket) WriteBuffers(p [][]byte) (int, error) {
	if err := c.prepareWrite(); err != nil {
		return c.result(0, err)
	}
	// WriteTo consumes its descriptors. Copy those, never the payload or the
	// caller's descriptors, and reuse their storage across frames.
	c.buffers = append(c.buffers[:0], p...)
	saved := c.buffers
	want := muxVectorBytes(p)
	n, err := c.buffers.WriteTo(c.tcp)
	clear(saved)
	c.buffers = saved[:0]
	return c.writeResult(int(n), want, err)
}
