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
	reader  io.Reader
	writeMu sync.Mutex
}

func (c *webSocketStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if c.reader == nil {
			kind, reader, err := c.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				return 0, &net.OpError{Op: "read", Net: "websocket", Err: errWSBinaryRequired}
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
		return n, err
	}
}

func (c *webSocketStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	w, err := c.conn.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(p)
	if err != nil {
		_ = w.Close()
		return n, err
	}
	return n, w.Close()
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
