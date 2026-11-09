package client

import (
	"context"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils/network"
)

func TestHealthSteeringWaitsForAProbeCapableFallback(t *testing.T) {
	e := network.NewEndpoints("primary:443", "backup:443")
	e.SetSpread(true)
	e.Rotate() // the primary failed the datagram tunnel handshake
	e.SetHealthRouting(nil, nil, false)
	stop := startHealthSteering(t.Context(), e, time.Millisecond, nil)
	t.Cleanup(stop)

	if e.Current() != "backup:443" || e.Next() != "backup:443" || e.Spread() {
		t.Fatal("suspended scoring lost the datagram route or enabled spreading")
	}
	if e.Rotate() != "primary:443" {
		t.Fatal("ordinary dial-failure rotation was disabled")
	}

	measured := make(chan struct{}, 1)
	e.SetHealthRouting(func(context.Context, string, int) network.EndpointScore {
		select {
		case measured <- struct{}{}:
		default:
		}
		return network.EndpointScore{Reachable: true, Score: 10}
	}, nil, false)
	select {
	case <-measured:
	case <-time.After(time.Second):
		t.Fatal("scoring did not start after the switch to a probe-capable carrier")
	}
}

type healthRunner struct {
	running  bool
	restarts int
}

func (r *healthRunner) Running() bool { return r.running }
func (r *healthRunner) Restart()      { r.restarts++ }

func TestHealthSwitchReconnectsActiveTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &healthRunner{running: true}
	switchPath := healthReconnect(ctx, r)
	switchPath("192.0.2.1:443", "[2001:db8::1]:443")
	if r.restarts != 1 {
		t.Fatal("health switch did not replace the active control channel and pool")
	}
	r.running = false
	switchPath("[2001:db8::1]:443", "192.0.2.1:443")
	if r.restarts != 1 {
		t.Fatal("disconnected tunnel should use its existing retry loop")
	}
	r.running = true
	cancel()
	switchPath("192.0.2.1:443", "[2001:db8::1]:443")
	if r.restarts != 1 {
		t.Fatal("retired transport was restarted after cancellation")
	}
}
