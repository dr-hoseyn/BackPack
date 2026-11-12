package network

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"time"
)

type wsControlSink struct{ net.Conn }

func (*wsControlSink) Write(p []byte) (int, error)      { return len(p), nil }
func (*wsControlSink) Read([]byte) (int, error)         { return 0, io.EOF }
func (*wsControlSink) SetWriteDeadline(time.Time) error { return nil }
func (*wsControlSink) SetReadDeadline(time.Time) error  { return nil }
func (*wsControlSink) SetDeadline(time.Time) error      { return nil }
func (*wsControlSink) Close() error                     { return nil }

type wsControlUpgrade struct {
	conn   net.Conn
	header http.Header
}

func (w wsControlUpgrade) Header() http.Header       { return w.header }
func (wsControlUpgrade) Write(p []byte) (int, error) { return len(p), nil }
func (wsControlUpgrade) WriteHeader(int)             {}
func (w wsControlUpgrade) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}
