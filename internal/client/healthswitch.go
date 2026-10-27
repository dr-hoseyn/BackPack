package client

import (
	"context"
	"fmt"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
)

func datagramHealthProbe(_ context.Context, addr string, _ int) network.EndpointScore {
	return network.EndpointScore{Addr: addr, Reachable: false, Score: 1e9}
}

// ICMP proves host reachability, not that a datagram tunnel answers its port.
// Keep a working KCP/QUIC/UDP control channel on its selected backup rather
// than interrupting users to revisit a pingable, blackholed primary. Ordinary
// dial-failure rotation remains enabled for every transport.
func startHealthSteering(ctx context.Context, tr config.TransportType, endpoints *network.Endpoints, interval time.Duration, log func(string)) func() {
	endpoints.SetSpread(false)
	if !tcpPortTransport(tr) {
		if log != nil {
			log("health-based address switching requires a TCP tunnel-port probe; datagram dial-failure failover remains enabled")
		}
		return func() {}
	}
	healthCtx, cancel := context.WithCancel(ctx)
	stopped := endpoints.EnableHealthSteering(healthCtx, interval, log)
	if log != nil {
		log(fmt.Sprintf("automatic health failover enabled across %d endpoints", endpoints.Len()))
	}
	return func() { cancel(); <-stopped }
}

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
