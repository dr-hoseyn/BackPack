package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Only the measurements are controlled: both IP families, both real transport
// endpoints, their control channels, mux pools and application bytes are real.
func TestAddressHealthFailoverMovesActiveMuxBetweenIPv4AndIPv6(t *testing.T) {
	if runtime.GOOS != "linux" || !ipv6Available(t) {
		t.Skip("requires Linux and IPv6 loopback")
	}
	for _, transport := range []string{"tcpmux", "wsmux"} {
		t.Run(transport, func(t *testing.T) {
			dir := t.TempDir()
			phase := filepath.Join(dir, "degraded")
			script := `#!/bin/sh
for host do :; done
rtt=10
if [ "$host" = "::1" ]; then rtt=100; fi
if [ -e "$BP_HEALTH_PHASE" ]; then
  rtt=100
  if [ "$host" = "::1" ]; then rtt=10; fi
fi
echo '5 packets transmitted, 5 received, 0% packet loss'
echo "rtt min/avg/max/mdev = $rtt/$rtt/$rtt/0 ms"
`
			if err := os.WriteFile(filepath.Join(dir, "ping"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("BP_HEALTH_PHASE", phase)
			backend := startEchoBackend(t)
			control, entry := freePort(t), freePort(t)
			srv := baseServerConfig(transport, control, entry, backend.addr, "dual-family-health-0123456789abcdef")
			v4 := newControlledPathOn(t, srv.BindAddr, "tcp4", "127.0.0.1:0")
			v6 := newControlledPathOn(t, srv.BindAddr, "tcp6", "[::1]:0")
			cli := baseClientConfig(transport, v4.addr, srv.Token, []string{v6.addr})
			cli.HealthFailover = true
			tun := runPair(t, srv, cli, entry, control)
			if err := tun.waitReady(tunnelReadyTimeout); err != nil {
				t.Fatal(err)
			}
			if v4.bytes.Load() == 0 || v6.bytes.Load() != 0 {
				t.Fatal("tunnel did not initially use only IPv4")
			}
			if err := os.WriteFile(phase, nil, 0600); err != nil {
				t.Fatal(err)
			}
			waitAddressTraffic(t, tun, v6, 45*time.Second)
			before := v4.bytes.Load()
			if err := os.Remove(phase); err != nil {
				t.Fatal(err)
			}
			waitAddressTrafficAfter(t, tun, v4, before, 45*time.Second)
		})
	}
}

func waitAddressTraffic(t *testing.T, tun *tunnel, path *controlledPath, timeout time.Duration) {
	t.Helper()
	waitAddressTrafficAfter(t, tun, path, 0, timeout)
}

func waitAddressTrafficAfter(t *testing.T, tun *tunnel, path *controlledPath, before int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	payload := randomPayload(t, 64*1024)
	var last error
	for time.Now().Before(deadline) {
		start := path.bytes.Load()
		if last = tun.roundTrip(payload); last == nil && start > before && path.bytes.Load()-start >= int64(len(payload)) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(fmt.Errorf("application traffic did not move to %s (bytes=%d): %v", path.addr, path.bytes.Load(), last))
}
