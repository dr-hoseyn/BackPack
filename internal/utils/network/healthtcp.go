package network

import (
	"context"
	"math"
	"time"
)

// TCPHealthProbe checks the actual tunnel port through the configured source,
// interface, routing mark and proxy. Direct routes keep ICMP quality metrics
// when available; blocked or unavailable ICMP falls back to TCP connect timing.
func TCPHealthProbe(out *Outbound, timeout time.Duration) func(context.Context, string, int) EndpointScore {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return func(ctx context.Context, addr string, samples int) EndpointScore {
		if out.IsSet() {
			return scoreTCP(ctx, out, addr, samples, timeout)
		}
		ping := scoreEndpoint(ctx, addr, samples)
		if ctx.Err() != nil {
			return ping
		}
		if ping.Reachable {
			port := scoreTCP(ctx, out, addr, 1, timeout)
			if !port.Reachable {
				return port
			}
			return ping
		}
		return scoreTCP(ctx, out, addr, samples, timeout)
	}
}

func scoreTCP(ctx context.Context, out *Outbound, addr string, samples int, timeout time.Duration) EndpointScore {
	if samples <= 0 {
		samples = 5
	}
	s := EndpointScore{Addr: addr, Score: 1e9, LossPct: 100}
	var sum, squares float64
	success, attempted := 0, 0
	for i := 0; i < samples && ctx.Err() == nil; i++ {
		started := time.Now()
		c, err := TcpDialerVia(ctx, out, addr, timeout, 30*time.Second, true, 1, 0, 0, 0)
		attempted++
		if err != nil {
			continue
		}
		ms := float64(time.Since(started)) / float64(time.Millisecond)
		c.Close()
		sum += ms
		squares += ms * ms
		success++
	}
	if success == 0 {
		return s
	}
	s.Reachable = true
	s.RTTms = sum / float64(success)
	s.JitterMs = math.Sqrt(math.Max(0, squares/float64(success)-s.RTTms*s.RTTms))
	s.LossPct = 100 * float64(attempted-success) / float64(attempted)
	s.Score = computeScore(s.RTTms, s.JitterMs, s.LossPct)
	return s
}
