package naive

import (
	"sync"
	"time"
)

// Coalesce concurrent metrics reads without retaining a previous helper's data.
// Failed samples get a shorter retry window so transient /proc races recover.
type helperResourceCache struct {
	mu       sync.Mutex
	pid      int32
	at       time.Time
	rss      uint64
	cpu      float64
	measured bool
}

func (c *helperResourceCache) read(pid int32, now time.Time, sample func() (uint64, float64, bool)) (uint64, float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pid <= 0 {
		c.pid, c.at, c.rss, c.cpu, c.measured = 0, time.Time{}, 0, 0, false
		return 0, 0, false
	}
	interval := 5 * time.Second
	if !c.measured {
		interval = time.Second
	}
	if c.pid == pid && !c.at.IsZero() && !now.Before(c.at) && now.Sub(c.at) < interval {
		return c.rss, c.cpu, c.measured
	}
	c.rss, c.cpu, c.measured = sample()
	if !c.measured {
		c.rss, c.cpu = 0, 0
	}
	c.pid, c.at = pid, now
	return c.rss, c.cpu, c.measured
}
