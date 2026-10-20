package e2e

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/manage/tunnelspec"
)

// Opt-in measurement of actual preset-rendered tunnels, including TLS and
// concurrent traffic. Results describe this host's loopback, not a WAN promise.
func TestBulkProfileTransportCapacity(t *testing.T) {
	if os.Getenv("BP_CAPACITY_BENCHMARK") == "" {
		t.Skip("set BP_CAPACITY_BENCHMARK=1 to measure")
	}
	payload := randomPayload(t, 16<<20)
	for _, tr := range []string{"tcp", "stealth", "tcpmux", "ws", "wss", "wsmux", "wssmux", "kcp", "quic", "udp"} {
		t.Run(tr, func(t *testing.T) {
			backend := startEchoBackend(t)
			tunnelPort, entryPort := freePort(t), freePort(t)
			const token = "bulk-capacity-measurement-token-0123456789"
			cfg := config.Config{
				Server: *baseServerConfig(tr, tunnelPort, entryPort, backend.addr, token),
				Client: *baseClientConfig(tr, fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil),
			}
			for _, role := range []string{"server", "client"} {
				spec := tunnelspec.Spec{Role: role, Transport: tr, Token: token, SkipOptz: true,
					BindAddr:   fmt.Sprintf("127.0.0.1:%d", tunnelPort),
					RemoteAddr: fmt.Sprintf("127.0.0.1:%d", tunnelPort),
					Ports:      []string{fmt.Sprintf("%d=%s", entryPort, backend.addr)},
				}
				tunnelspec.ApplyPreset(&spec, tunnelspec.PresetThroughput)
				if _, err := toml.Decode(spec.Render(), &cfg); err != nil {
					t.Fatal(err)
				}
			}
			cfg.Server.LogLevel, cfg.Client.LogLevel = "error", "error"
			tun := runPair(t, &cfg.Server, &cfg.Client, entryPort, tunnelPort)
			if err := tun.waitReady(tunnelReadyTimeout); err != nil {
				t.Fatal(err)
			}
			for _, streams := range []int{1, 8} {
				start := time.Now()
				results := make(chan error, streams)
				for i := 0; i < streams; i++ {
					go func() { results <- tun.roundTrip(payload) }()
				}
				for i := 0; i < streams; i++ {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				seconds := time.Since(start).Seconds()
				// Report one-way-equivalent application goodput. Echo traffic
				// also crosses back, so full-duplex wire traffic is about twice it.
				t.Logf("CAPACITY transport=%s streams=%d goodput_mbps=%.1f seconds=%.3f", tr, streams,
					float64(streams*len(payload)*8)/seconds/1e6, seconds)
			}
		})
	}
}
