package e2e

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/client"
	"github.com/backpack/backpack/internal/server"
)

// Opt-in two-host smoke test. Only dedicated temporary ports are used; no
// systemd, firewall, qdisc, existing config or running service is changed.
func TestWSSMuxDedicatedLivePath(t *testing.T) {
	role := os.Getenv("BP_WSSMUX_LIVE_ROLE")
	if role == "" {
		t.Skip("set BP_WSSMUX_LIVE_ROLE, BP_WSSMUX_LIVE_REMOTE and a fresh BP_WSSMUX_LIVE_TOKEN")
	}
	token := os.Getenv("BP_WSSMUX_LIVE_TOKEN")
	if len(token) < 32 {
		t.Fatal("live fixture requires a fresh strong token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	port := func(key string, fallback int) int {
		if raw := os.Getenv(key); raw != "" {
			v, e := strconv.Atoi(raw)
			if e != nil || v < 1024 || v > 65535 {
				t.Fatalf("invalid %s", key)
			}
			return v
		}
		return fallback
	}
	backendPort := port("BP_WSSMUX_LIVE_BACKEND_PORT", 21992)
	tunnelPort := port("BP_WSSMUX_LIVE_TUNNEL_PORT", 21991)
	entryPort := port("BP_WSSMUX_LIVE_ENTRY_PORT", 21993)
	switch role {
	case "client":
		backend, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)))
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close()
		go func() {
			for {
				c, e := backend.Accept()
				if e != nil {
					return
				}
				go func() { defer c.Close(); io.Copy(c, c) }()
			}
		}()
		cfg := baseClientConfig("wssmux", os.Getenv("BP_WSSMUX_LIVE_REMOTE"), token, nil)
		cfg.ConnectionPool, cfg.WSMuxMaxSessions = 2, 2
		cfg.LogLevel = "error"
		engine := client.NewClient(cfg, ctx)
		wg.Go(engine.Start)
		<-ctx.Done()
	case "server":
		cfg := baseServerConfig("wssmux", tunnelPort, entryPort, net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)), token)
		cfg.BindAddr = net.JoinHostPort("0.0.0.0", strconv.Itoa(tunnelPort))
		cfg.Ports = []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)) + "=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort))}
		cfg.MuxCon, cfg.MaxConnections, cfg.BandwidthMbps = 8, 4, 20
		cfg.LogLevel = "error"
		engine := server.NewServer(cfg, ctx)
		wg.Go(engine.Start)
		tun := &tunnel{Entry: net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort))}
		if err := tun.waitReady(25 * time.Second); err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{128, 64 << 10, 256 << 10} {
			start := time.Now()
			if err := tun.roundTrip(randomPayload(t, size)); err != nil {
				t.Fatal(err)
			}
			t.Logf("LIVE_WSSMUX bytes_each_direction=%d elapsed_ms=%d verified=true", size, time.Since(start).Milliseconds())
		}
	default:
		t.Fatal("unknown live role")
	}
}
