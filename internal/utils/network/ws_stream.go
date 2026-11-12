package network

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketStream carries an ordered byte stream in RFC 6455 binary messages.
// It adds no batching timer: a small write leaves immediately, while Gorilla's
// pooled 64 KiB writer coalesces the frame header with bulk data. smux owns one
// reader and one writer; its keepalives also keep control frames being read.
func WebSocketStream(conn *websocket.Conn) net.Conn {
	return &webSocketStream{conn: conn}
}

const WSMuxBinaryProtocol = "binary"

const (
	wsMuxWindowUpdateCommand = 4
	wsMuxWindowUpdateSize    = 8
)

// Older peers did not frame their data. Select the format only after the
// authenticated upgrade has acknowledged the standard binary subprotocol.
func WSMuxStream(conn *websocket.Conn) net.Conn {
	if conn.Subprotocol() == WSMuxBinaryProtocol {
		return WebSocketStream(conn)
	}
	return conn.NetConn()
}

// WSMuxAnnouncementStream is only for the reverse server, which always follows
// OpenStream with the target address before relaying application data. Retain
// one copied SYN until that next write, so the two small frames share a single
// WebSocket message and TLS write. No timer or application data is buffered.
// Generic mux users must keep WSMuxStream: they may wait for AcceptStream before
// writing. Legacy raw peers retain their original immediate-write behavior.
func WSMuxAnnouncementStream(conn *websocket.Conn) net.Conn {
	if conn.Subprotocol() == WSMuxBinaryProtocol {
		return &webSocketStream{conn: conn, announceOpen: true}
	}
	return conn.NetConn()
}

type webSocketStream struct {
	conn     *websocket.Conn
	readMu   sync.Mutex
	reader   io.Reader
	readErr  error
	writeMu  sync.Mutex
	closed   atomic.Bool
	writeErr error // accessed only under writeMu
	// Only the writeMu owner reuses this fixed-size control metadata.
	controlFrame [smuxHeaderSize + wsMuxWindowUpdateSize]byte
	announceOpen bool
	openPending  bool
	openFrame    [smuxHeaderSize]byte
}

func (c *webSocketStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	// WebSocket framing cannot resume after a read failure. Keep callers
	// from skipping a rejected message or repeatedly entering NextReader.
	if c.readErr != nil {
		return 0, c.readErr
	}
	for {
		if c.reader == nil {
			kind, reader, err := c.conn.NextReader()
			if err != nil {
				c.readErr = err
				c.Close()
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				c.readErr = &net.OpError{Op: "read", Net: "websocket", Err: errWSBinaryRequired}
				c.Close()
				return 0, c.readErr
			}
			c.reader = reader
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			c.reader = nil
			c.readErr = err
			c.Close()
		}
		return n, err
	}
}

func (c *webSocketStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return c.WriteBuffers([][]byte{p})
}

// WriteBuffers lets smux write its eight-byte header and payload into the same
// binary message without first copying them into a private 64 KiB/session
// staging buffer. All bytes are consumed synchronously before returning.
func (c *webSocketStream) WriteBuffers(v [][]byte) (int, error) {
	hasData := false
	for _, p := range v {
		if len(p) > 0 {
			hasData = true
			break
		}
	}
	if !hasData {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	var prefix []byte
	if c.openPending {
		prefix = c.openFrame[:]
		c.openPending = false
	} else if c.announceOpen && len(v) == 2 && len(v[0]) == smuxHeaderSize && len(v[1]) == 0 {
		header := v[0]
		if (header[0] == 1 || header[0] == 2) && header[1] == 0 && header[2] == 0 && header[3] == 0 {
			// smux reuses its header as soon as this method returns. Never
			// retain the caller's slice, including on cancellation/failure.
			copy(c.openFrame[:], header)
			c.openPending = true
			return len(header), nil
		}
	}
	// These smux controls contain no application payload. Gorilla's server
	// WriteMessage keeps its writer on the stack. Join a v2 window update in
	// per-carrier storage rather than allocating a temporary 16-byte slice.
	// All bytes are consumed synchronously while writeMu remains held.
	if prefix == nil && len(v) == 2 && len(v[0]) == smuxHeaderSize {
		header, body := v[0], v[1]
		var control []byte
		if len(body) == 0 {
			control = header
		} else if len(body) == wsMuxWindowUpdateSize && header[0] == 2 &&
			header[1] == wsMuxWindowUpdateCommand && header[2] == wsMuxWindowUpdateSize && header[3] == 0 {
			copy(c.controlFrame[:smuxHeaderSize], header)
			copy(c.controlFrame[smuxHeaderSize:], body)
			control = c.controlFrame[:]
		}
		if control != nil {
			if err := c.conn.WriteMessage(websocket.BinaryMessage, control); err != nil {
				return c.failWrite(0, err)
			}
			return len(control), nil
		}
	}
	w, err := c.conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return c.failWrite(0, err)
	}
	if prefix != nil {
		if _, err := w.Write(prefix); err != nil {
			c.Close()
			_ = w.Close()
			return c.failWrite(0, err)
		}
	}
	n := 0
	for _, p := range v {
		written, err := w.Write(p)
		n += written
		if err != nil {
			c.Close()
			_ = w.Close()
			return c.failWrite(n, err)
		}
	}
	err = w.Close()
	if err != nil {
		// A broken frame cannot resume. Close the socket to wake an
		// otherwise idle reader and let the mux session retire.
		return c.failWrite(n, err)
	}
	return n, err
}

func (c *webSocketStream) failWrite(n int, err error) (int, error) {
	c.writeErr = err
	c.Close()
	return n, err
}

func (c *webSocketStream) Close() error {
	c.closed.Store(true)
	return c.conn.Close()
}
func (c *webSocketStream) LocalAddr() net.Addr  { return c.conn.LocalAddr() }
func (c *webSocketStream) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetDeadline acts on the socket, rather than Gorilla's writer field, so an
// external cancellation/deadline can interrupt a blocked write without a race.
func (c *webSocketStream) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
func (c *webSocketStream) SetReadDeadline(t time.Time) error {
	return c.conn.NetConn().SetReadDeadline(t)
}
func (c *webSocketStream) SetWriteDeadline(t time.Time) error {
	if d, ok := c.conn.NetConn().(interface{ setStreamWriteDeadline(time.Time) error }); ok {
		return d.setStreamWriteDeadline(t)
	}
	return c.conn.NetConn().SetWriteDeadline(t)
}
