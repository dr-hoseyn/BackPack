package transport

import "sync/atomic"

// Limits configures the maximum number of open forwarded connections.
type Limits struct {
	// MaxConnections caps how many forwarded connections may be open at once.
	// Zero means unlimited.
	MaxConnections int
}

// limiter tracks open connections when a cap is configured.
type limiter struct {
	maxConns int32
	active   atomic.Int32
}

// newLimiter builds a limiter, or nil when nothing is limited.
func newLimiter(l Limits) *limiter {
	if l.MaxConnections <= 0 {
		return nil
	}
	return &limiter{maxConns: int32(l.MaxConnections)}
}

// acquire reserves a connection slot, reporting whether one was available.
func (l *limiter) acquire() bool {
	if l == nil || l.maxConns <= 0 {
		return true
	}
	if l.active.Add(1) > l.maxConns {
		l.active.Add(-1)
		return false
	}
	return true
}

// release returns a connection slot.
func (l *limiter) release() {
	if l == nil || l.maxConns <= 0 {
		return
	}
	l.active.Add(-1)
}
