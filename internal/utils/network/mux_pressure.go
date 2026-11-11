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
	return tcpMuxPressureSampler(func() (int, bool) { return tcpMuxSocketPressure(conn) })
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
