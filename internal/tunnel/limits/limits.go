// Package limits caps concurrent forwarded connections.
package limits

import "sync/atomic"

// Config describes the caps applied to one tunnel.
type Config struct {
	// MaxConnections caps how many forwarded connections may be open at once.
	// Zero means unlimited.
	MaxConnections int
}

// Limiter enforces a Config. A nil *Limiter enforces nothing, so every method
// is safe on one and the unlimited path costs a single comparison.
type Limiter struct {
	maxConns int32
	active   atomic.Int32
}

// New builds a limiter, or nil when nothing is limited.
func New(c Config) *Limiter {
	if c.MaxConnections <= 0 {
		return nil
	}
	return &Limiter{maxConns: int32(c.MaxConnections)}
}

// Acquire reserves a connection slot, reporting whether one was available.
// Every Acquire that returns true must be paired with a Release.
func (l *Limiter) Acquire() bool {
	if l == nil || l.maxConns <= 0 {
		return true
	}
	if l.active.Add(1) > l.maxConns {
		l.active.Add(-1)
		return false
	}
	return true
}

// Release returns a connection slot.
func (l *Limiter) Release() {
	if l == nil || l.maxConns <= 0 {
		return
	}
	l.active.Add(-1)
}

// Active is how many slots are currently held, for diagnostics.
func (l *Limiter) Active() int {
	if l == nil {
		return 0
	}
	return int(l.active.Load())
}
