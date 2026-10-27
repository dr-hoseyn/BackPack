package client

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
)

func TestDatagramHealthFailoverDoesNotSteerToAPingableClosedPort(t *testing.T) {
	for _, tr := range []config.TransportType{config.KCP, config.QUIC, config.XDI, config.PCK, config.SPOOF} {
		t.Run(string(tr), func(t *testing.T) {
			e := network.NewEndpoints("primary:443", "backup:443")
			e.SetSpread(true)
			e.Rotate() // the primary failed the real tunnel handshake
			var probes atomic.Int32
			e.SetHealthProbe(func(context.Context, string, int) network.EndpointScore {
				probes.Add(1)
				return network.EndpointScore{Reachable: true, Score: 0}
			})
			stop := startHealthSteering(t.Context(), tr, e, time.Millisecond, nil)
			time.Sleep(10 * time.Millisecond)
			stop()
			if probes.Load() != 0 || e.Current() != "backup:443" || e.Spread() {
				t.Fatal("ICMP-only scoring replaced the working datagram backup or enabled spreading")
			}
			if e.Rotate() != "primary:443" {
				t.Fatal("ordinary dial-failure rotation was disabled")
			}
		})
	}
}

func TestTCPHealthSteeringStillMeasuresAndStops(t *testing.T) {
	for _, tr := range []config.TransportType{config.WSSMUX, config.UDP} {
		t.Run(string(tr), func(t *testing.T) {
			e := network.NewEndpoints("primary:443", "backup:443")
			measured := make(chan struct{}, 8)
			e.SetHealthProbe(func(context.Context, string, int) network.EndpointScore {
				select {
				case measured <- struct{}{}:
				default:
				}
				return network.EndpointScore{Reachable: true, Score: 10}
			})
			stop := startHealthSteering(t.Context(), tr, e, time.Millisecond, nil)
			select {
			case <-measured:
			case <-time.After(time.Second):
				t.Fatal("TCP health scoring did not start")
			}
			stop() // joins the scorer and its probe workers
		})
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
