package handlers

import (
	"io"
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
	// matching CountedConn.Read on the buffered path. The destination is a real
	// TCP socket: a successful Write consumes the whole frame synchronously.
	if w.counted {
		metrics.AddBytes(uint64(len(p)), 0)
	}
	n, err := w.to.Write(p)
	if err == nil && n != len(p) {
		// smux.WriterTo does not itself reject a short successful write.
		// Never let it recycle the frame and silently discard the remainder.
		err = io.ErrShortWrite
	}
	if err != nil {
		w.log.Trace("unable to write mux receive frame: ", err)
		w.from.Close()
		w.to.Close()
		return n, err
	}
	if w.log.IsLevelEnabled(logrus.TraceLevel) {
		w.log.Tracef("read data: %d bytes, written data: %d bytes", len(p), n)
	}
	if w.sniffer {
		w.usage.AddOrUpdatePort(w.port, uint64(n))
	}
	return n, nil
}
