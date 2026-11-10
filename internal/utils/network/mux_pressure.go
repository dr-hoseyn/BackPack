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
	var mu sync.Mutex
	var sampled time.Time
	var penalty int
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if sampled.IsZero() || now.Sub(sampled) >= 200*time.Millisecond {
			sampled = now
			penalty = min(int(tcpMuxDelay(conn)/(5*time.Millisecond)), 64)
		}
		return penalty
	}
}
