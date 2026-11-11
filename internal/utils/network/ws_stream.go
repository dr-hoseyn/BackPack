package network

import (
	"io"
	"net"
	"sync"
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

// Older peers did not frame their data. Select the format only after the
// authenticated upgrade has acknowledged the standard binary subprotocol.
func WSMuxStream(conn *websocket.Conn) net.Conn {
	if conn.Subprotocol() == WSMuxBinaryProtocol {
		return WebSocketStream(conn)
	}
	return conn.NetConn()
}

type webSocketStream struct {
	conn    *websocket.Conn
	readMu  sync.Mutex
	reader  io.Reader
	readErr error
	writeMu sync.Mutex
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
				c.conn.Close()
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				c.readErr = &net.OpError{Op: "read", Net: "websocket", Err: errWSBinaryRequired}
				c.conn.Close()
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
			c.conn.Close()
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
	// smux sends SYN/FIN/keepalive as an eight-byte header with no payload.
	// Gorilla's server-side WriteMessage path keeps its message writer on the
	// stack. There is no payload to stage or acknowledge on a failed control
	// write; the watched mux socket retires the carrier on any write error.
	if len(v) == 2 && len(v[0]) == smuxHeaderSize && len(v[1]) == 0 {
		if err := c.conn.WriteMessage(websocket.BinaryMessage, v[0]); err != nil {
			c.conn.Close()
			return 0, err
		}
		return smuxHeaderSize, nil
	}
	w, err := c.conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		c.conn.Close()
		return 0, err
	}
	n := 0
	for _, p := range v {
		written, err := w.Write(p)
		n += written
		if err != nil {
			c.conn.Close()
			_ = w.Close()
			return n, err
		}
	}
	err = w.Close()
	if err != nil {
		// A broken frame cannot resume. Close the socket to wake an
		// otherwise idle reader and let the mux session retire.
		c.conn.Close()
	}
	return n, err
}

func (c *webSocketStream) Close() error         { return c.conn.Close() }
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
