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
	conn = tcpMuxPressureConn(conn)
	return tcpMuxPressureSampler(func() (int, bool) { return tcpMuxSocketPressure(conn) })
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
	var mu sync.Mutex
	var sampled time.Time
	var penalty int
	var backlogged bool
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if sampled.IsZero() || now.Sub(sampled) >= 200*time.Millisecond {
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
