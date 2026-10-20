package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"runtime/pprof"
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
	for _, tr := range []string{"tcp", "stealth", "tcpmux", "ws", "wss", "wsmux", "wssmux", "kcp", "quic"} {
		t.Run(tr, func(t *testing.T) {
			tun := bulkProfileTunnel(t, tr)
			// Readiness proves the first session, not a fully primed 64-session
			// pool. Exclude initial handshakes from the steady-state goodput
			// measurement; the separate admission and soak tests cover startup.
			time.Sleep(time.Second)
			if err := tun.roundTrip(payload[:1<<20]); err != nil {
				t.Fatal(err)
			}
			for _, streams := range []int{1, 8} {
				var snapshot *time.Timer
				if os.Getenv("BP_CAPACITY_DIAGNOSTICS") != "" {
					snapshot = time.AfterFunc(5*time.Second, func() {
						fmt.Fprintf(os.Stderr, "CAPACITY_STALL transport=%s streams=%d\n", tr, streams)
						pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
						if sockets, err := exec.Command("ss", "-tinmp").CombinedOutput(); err == nil {
							fmt.Fprintf(os.Stderr, "CAPACITY_TCP_SOCKETS\n%s\n", sockets)
						}
					})
					t.Cleanup(func() { snapshot.Stop() })
				}
				start := time.Now()
				results := make(chan error, streams)
				for i := 0; i < streams; i++ {
					go func() { results <- tun.roundTrip(payload) }()
				}
				for i := 0; i < streams; i++ {
					if err := <-results; err != nil {
						if snapshot != nil {
							snapshot.Stop()
						}
						t.Fatal(err)
					}
				}
				if snapshot != nil {
					snapshot.Stop()
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

// This control deliberately bypasses every Backpack component. A timeout here
// identifies a host TCP/echo-fixture issue rather than a carrier regression.
func TestDirectBackendBulkCapacity(t *testing.T) {
	if os.Getenv("BP_CAPACITY_BENCHMARK") == "" {
		t.Skip("set BP_CAPACITY_BENCHMARK=1 to measure")
	}
	backend := startEchoBackend(t)
	tun := &tunnel{Entry: backend.addr}
	payload := randomPayload(t, 16<<20)
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- tun.roundTrip(payload) }()
	}
	for i := 0; i < 8; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func bulkProfileTunnel(t *testing.T, tr string) *tunnel {
	return bulkProfileConfigured(t, tr, nil)
}

func bulkProfileConfigured(t *testing.T, tr string, tune func(*config.Config)) *tunnel {
	t.Helper()
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
	if tune != nil {
		tune(&cfg)
	}
	cfg.Server.LogLevel, cfg.Client.LogLevel = "error", "error"
	tun := runPair(t, &cfg.Server, &cfg.Client, entryPort, tunnelPort)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatal(err)
	}
	return tun
}

// Reproducible tuning comparison: a large ARQ queue can spend more CPU scanning
// ACKs than it earns in bandwidth. This diagnostic is deliberately opt-in.
func TestKCPBulkWindowCandidates(t *testing.T) {
	if os.Getenv("BP_KCP_TUNING_BENCHMARK") == "" {
		t.Skip("set BP_KCP_TUNING_BENCHMARK=1")
	}
	payload := randomPayload(t, 8<<20)
	for _, nc := range []int{1, 0} {
		for _, window := range []int{2048, 4096, 16384} {
			t.Run(fmt.Sprintf("nc%d-window%d", nc, window), func(t *testing.T) {
				tun := bulkProfileConfigured(t, "kcp", func(cfg *config.Config) {
					cfg.Server.KCPConfig.SndWnd, cfg.Server.KCPConfig.RcvWnd = window, window
					cfg.Client.KCPConfig.SndWnd, cfg.Client.KCPConfig.RcvWnd = window, window
					cfg.Server.KCPConfig.NoCongestion, cfg.Client.KCPConfig.NoCongestion = nc, nc
				})
				if err := tun.roundTrip(payload[:1<<20]); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				results := make(chan error, 8)
				for i := 0; i < 8; i++ {
					go func() { results <- tun.roundTrip(payload) }()
				}
				for i := 0; i < 8; i++ {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
				t.Logf("KCP_TUNING nc=%d window=%d streams=8 goodput_mbps=%.1f", nc, window, float64(8*len(payload)*8)/time.Since(start).Seconds()/1e6)
			})
		}
	}
}
