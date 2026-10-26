package client

import "context"

// runner reports whether the control channel is up and can reconnect it after
// a health-based address switch. Every reverse transport supports both.
type runner interface {
	Running() bool
	Restart()
}

// A scoring decision must renew the control channel and pool as well as change
// the next dial address. Otherwise mux sessions can stay on the degraded path
// indefinitely. Disconnected transports already have their own retry loop.
func healthReconnect(ctx context.Context, r runner) func(string, string) {
	return func(_, _ string) {
		if ctx.Err() == nil && r.Running() {
			r.Restart()
		}
	}
}
