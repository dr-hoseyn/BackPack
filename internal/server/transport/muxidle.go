package transport

import (
	"sync"
	"time"
)

// Do not recycle warm TCP capacity during ongoing transfers: that reserve is
// what keeps fresh users away from a busy connection's loss-recovery queue.
// Idle-session timers share this sampler, so no goroutine or per-byte clock
// read is added. A first observation starts the quiet period conservatively.
type muxIdleTraffic struct {
	mu      sync.Mutex
	in, out uint64
	since   time.Time
}

func (p *muxIdleTraffic) quiet(now time.Time, in, out uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.since.IsZero() || in != p.in || out != p.out {
		p.in, p.out, p.since = in, out, now
		return false
	}
	return now.Sub(p.since) >= 30*time.Second
}
