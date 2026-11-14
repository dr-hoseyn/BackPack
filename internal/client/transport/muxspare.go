package transport

import "time"

// Experimental and opt-in. Demand growth, floor repair and retirement keep
// their established policy. Only throughput growth can be deferred, after a
// full 30 seconds with at least one configured pool of empty carriers and
// successful local pressure observations on every carrier. This is not a
// measurement of congestion at the remote endpoint.
type muxSpareGuard struct{ since time.Time }

func (g *muxSpareGuard) allowThroughputGrowth(now time.Time, enabled bool, s muxPoolSample, live, floor int) bool {
	if !enabled || floor < 1 || s.total != live || s.total == 0 ||
		s.total-s.active < floor || s.pressureObserved != s.total || s.pressured != 0 {
		g.since = time.Time{}
		return true
	}
	if g.since.IsZero() {
		g.since = now
	}
	return now.Sub(g.since) < 30*time.Second
}
