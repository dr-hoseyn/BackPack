package client

import (
	"context"
	"testing"
)

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
