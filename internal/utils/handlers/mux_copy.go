package handlers

import (
	"net"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/traffic"
	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

// A smux receive queue already owns complete frame buffers. Its WriterTo can
// send those straight to a TCP socket instead of copying through another relay
// buffer. Only unwrap accounting: source pacing and destination wrappers keep
// the regular buffered path, and a failed mux-to-mux write may retain its slice.
func transferMuxToTCP(from, to net.Conn, logger *logrus.Logger, usage *traffic.Usage, port int, sniffer bool) bool {
	source, counted := metrics.Uncount(from)
	stream, ok := source.(*smux.Stream)
	if !ok {
		return false
	}
	destination, _ := metrics.Uncount(to)
	if _, ok := destination.(*net.TCPConn); !ok {
		return false
	}
	w := &muxRelayWriter{from: from, to: to, log: logger, usage: usage, port: port, sniffer: sniffer, counted: counted}
	_, err := stream.WriteTo(w)
	finishDirection(from, to, err)
	return true
}

type muxRelayWriter struct {
	from, to net.Conn
	log      *logrus.Logger
	usage    *traffic.Usage
	port     int
	sniffer  bool
	counted  bool
}

func (w *muxRelayWriter) Write(p []byte) (int, error) {
	// Count a consumed tunnel frame even if the backend write then fails,
	// matching CountedConn.Read on the buffered path. writeAll preserves short
	// writes and per-port usage without allowing smux to recycle a live slice.
	if w.counted {
		metrics.AddBytes(uint64(len(p)), 0)
	}
	if !writeAll(w.from, w.to, p, w.log, w.usage, w.port, w.sniffer) {
		return 0, net.ErrClosed
	}
	return len(p), nil
}
