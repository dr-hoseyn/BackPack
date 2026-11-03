package handlers

import (
	"net"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/traffic"
	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

// Keep smux's Read flow-control timing: unlike WriterTo, it returns receive
// credit and sends window updates before the backend write can block. A small
// initial buffer retains little memory for quiet streams, then a full read
// promotes bulk forwarding to the regular 64 KiB pool without a batching timer.
// Only identify known wrappers; copies still pass through their accounting.
func transferMuxToTCP(from, to net.Conn, logger *logrus.Logger, usage *traffic.Usage, port int, sniffer bool) bool {
	source, _ := metrics.Uncount(from)
	_, ok := source.(*smux.Stream)
	if !ok {
		return false
	}
	destination, _ := metrics.Uncount(to)
	if _, ok := destination.(*net.TCPConn); !ok {
		return false
	}
	buffer := newMuxRelayCopyBuffer()
	// A real TCP Write consumes its slice synchronously, even on failure.
	// Unlike a mux destination, it cannot keep a queued reference to this buffer.
	defer buffer.release()
	copyBuffered(from, to, &buffer, logger, usage, port, sniffer)
	return true
}
