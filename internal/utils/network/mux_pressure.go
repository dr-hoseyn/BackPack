package network

import (
	"net"
	"sync"
	"time"
)

// TCPMuxPressure estimates the extra delay ahead of a fresh stream. The
// scheduler samples each carrier at most five times a second, without adding
// work to each application read or write.
func TCPMuxPressure(conn net.Conn) func() int {
	return muxPressure(conn, 200*time.Millisecond)
}

// WSMuxPressure observes a congestion change before another short-connection
// burst can reuse a stale healthy sample. Only admission requests sample it;
// this does not poll idle carriers or add work to application copies.
func WSMuxPressure(conn net.Conn) func() int {
	return muxPressure(conn, 50*time.Millisecond)
}

func muxPressure(conn net.Conn, interval time.Duration) func() int {
	conn = tcpMuxPressureConn(conn)
	return muxPressureSampler(func() (int, bool) { return tcpMuxSocketPressure(conn) }, interval)
}

func tcpMuxPressureConn(conn net.Conn) net.Conn {
	// Inspect the actual TCP socket underneath TLS/deadline wrappers. Never
	// unwrap the I/O path: application bytes still pass through encryption.
	for range 8 {
		switch wrapper := conn.(type) {
		case interface{ UnderlyingConn() net.Conn }:
			conn = wrapper.UnderlyingConn()
		case interface{ NetConn() net.Conn }:
			conn = wrapper.NetConn()
		default:
			return conn
		}
	}
	// A malformed wrapper chain is not evidence of congestion.
	return nil
}

func tcpMuxPressureSampler(sample func() (int, bool)) func() int {
	return muxPressureSampler(sample, 200*time.Millisecond)
}

func muxPressureSampler(sample func() (int, bool), interval time.Duration) func() int {
	var mu sync.Mutex
	var sampled time.Time
	var penalty int
	var backlogged bool
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if sampled.IsZero() || now.Sub(sampled) >= interval {
			sampled = now
			next, queued := sample()
			// The last rate sample may come from an idle heartbeat. Confirm
			// the backlog at the next sample before displacing warm capacity.
			if queued && !backlogged {
				next = min(next, 8)
			}
			penalty, backlogged = next, queued
		}
		return penalty
	}
}
