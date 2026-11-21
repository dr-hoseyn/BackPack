package e2e

import (
	"context"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/client"
	"github.com/backpack/backpack/internal/manage/tunnelspec"
	"github.com/backpack/backpack/internal/metrics"
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
	ctx, cancel := context.WithTimeout(context.Background(), 85*time.Second)
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
	throughput := os.Getenv("BP_WSSMUX_LIVE_THROUGHPUT") == "1"
	var profile tunnelspec.Spec
	tunnelspec.ApplyPreset(&profile, tunnelspec.PresetThroughput)
	switch role {
	case "client":
		if os.Getenv("BP_WSSMUX_LIVE_EXTERNAL_BACKEND") != "1" {
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
		}
		cfg := baseClientConfig("wssmux", os.Getenv("BP_WSSMUX_LIVE_REMOTE"), token, nil)
		cfg.ConnectionPool, cfg.WSMuxMaxSessions = 2, 2
		if throughput {
			cfg.ConnectionPool, cfg.WSMuxMaxSessions = profile.ConnectionPool, profile.ConnectionPool
			cfg.Keepalive = profile.KeepAlive
			cfg.MaxFrameSize, cfg.MaxReceiveBuffer, cfg.MaxStreamBuffer = profile.MuxFrameSize, profile.MuxRecvBuffer, profile.MuxStreamBuffer
			cfg.SO_RCVBUF, cfg.SO_SNDBUF = profile.SoRcvBuf, profile.SoSndBuf
		}
		cfg.LogLevel = "error"
		engine := client.NewClient(cfg, ctx)
		wg.Go(engine.Start)
		if throughput {
			deadline := time.Now().Add(25 * time.Second)
			for {
				live, _, _, _ := metrics.PoolState()
				if live >= profile.ConnectionPool {
					t.Logf("LIVE_WSSMUX warm_pool=%d profile=throughput", live)
					break
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					t.Fatalf("Throughput pool stalled at %d/%d", live, profile.ConnectionPool)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		<-ctx.Done()
	case "server":
		cfg := baseServerConfig("wssmux", tunnelPort, entryPort, net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)), token)
		cfg.BindAddr = net.JoinHostPort("0.0.0.0", strconv.Itoa(tunnelPort))
		cfg.Ports = []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)) + "=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort))}
		cfg.MuxCon, cfg.MaxConnections, cfg.BandwidthMbps = 8, 4, 20
		if throughput {
			cfg.ChannelSize, cfg.MuxCon = profile.ChannelSize, profile.MuxCon
			cfg.MaxConnections = 32
			cfg.Keepalive, cfg.Heartbeat = profile.KeepAlive, profile.Heartbeat
			cfg.MaxFrameSize, cfg.MaxReceiveBuffer, cfg.MaxStreamBuffer = profile.MuxFrameSize, profile.MuxRecvBuffer, profile.MuxStreamBuffer
			cfg.SO_RCVBUF, cfg.SO_SNDBUF = profile.SoRcvBuf, profile.SoSndBuf
		}
		cfg.LogLevel = "error"
		engine := server.NewServer(cfg, ctx)
		wg.Go(engine.Start)
		if os.Getenv("BP_WSSMUX_LIVE_SERVICE_ONLY") == "1" {
			<-ctx.Done()
			return
		}
		tun := &tunnel{Entry: net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort))}
		if err := tun.waitReady(25 * time.Second); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("BP_WSSMUX_LIVE_COMPARE") == "1" {
			if throughput {
				deadline := time.Now().Add(25 * time.Second)
				for {
					snapshot := metrics.NewCollector("", "", "", "", nil, nil).Snapshot()
					if snapshot.WSMuxAdmission != nil && snapshot.WSMuxAdmission.Carriers >= profile.ConnectionPool {
						break
					}
					if time.Now().After(deadline) || ctx.Err() != nil {
						t.Fatal("Throughput server did not admit its warm pool")
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
			compareWSSMuxLive(t, tun)
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

// The capped offered load is deliberately small enough for shared live hosts.
// Server-side timing includes TCP entry, mux admission and the verified echo.
func compareWSSMuxLive(t *testing.T, tun *tunnel) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", tun.Entry, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	block, small := randomPayload(t, 64<<10), randomPayload(t, 128)
	stop := make(chan struct{})
	type bulkSample struct {
		blocks int
		err    error
	}
	done := make(chan bulkSample, 1)
	started := make(chan struct{})
	interval, samples := 100*time.Millisecond, 40
	if os.Getenv("BP_WSSMUX_LIVE_FIXED_LOAD") == "1" {
		interval, samples = 350*time.Millisecond, 120
	}
	bulkStart := time.Now()
	go func() {
		var sample bulkSample
		defer func() { done <- sample }()
		close(started)
		for {
			begin := time.Now()
			if sample.err = wssmuxRecoveryEcho(conn, block); sample.err != nil {
				return
			}
			sample.blocks++
			select {
			case <-stop:
				return
			case <-time.After(max(0, interval-time.Since(begin))):
			}
		}
	}()
	<-started
	replies := make([]time.Duration, 0, samples)
	failures := 0
	for range samples {
		start := time.Now()
		if err := tun.roundTrip(small); err != nil {
			failures++
			t.Logf("fresh WSSMux echo failed: %v", err)
			if failures >= 3 {
				break
			}
		} else {
			replies = append(replies, time.Since(start))
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	bulk := <-done
	seconds := time.Since(bulkStart).Seconds()
	sort.Slice(replies, func(i, j int) bool { return replies[i] < replies[j] })
	if len(replies) == 0 || bulk.err != nil || bulk.blocks == 0 {
		t.Fatalf("missing verified traffic: replies=%d blocks=%d bulk_error=%v", len(replies), bulk.blocks, bulk.err)
	}
	percentile := func(p int) float64 {
		return float64(replies[(p*len(replies)+99)/100-1]) / float64(time.Millisecond)
	}
	t.Logf("LIVE_WSSMUX_COMPARE verified=%d failures=%d p50_ms=%.2f p95_ms=%.2f p99_ms=%.2f bulk_echo_mbps=%.3f bulk_bytes=%d",
		len(replies), failures, percentile(50), percentile(95), percentile(99),
		float64(bulk.blocks*len(block)*8)/seconds/1e6, bulk.blocks*len(block))
	if failures != 0 {
		t.Fatalf("%d fresh connections failed", failures)
	}
}
