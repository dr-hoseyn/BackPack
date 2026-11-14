package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in: this is intentionally separate from the short TCP soak and the
// 1000-user bounded-profile benchmark. Copy this exact fixture to both refs
// when comparing revisions; it uses only public configuration and helpers.
func TestWSSMuxScaleSoak(t *testing.T) {
	if os.Getenv("BP_WSSMUX_SCALE_SOAK") == "" {
		t.Skip("set BP_WSSMUX_SCALE_SOAK=1")
	}
	setting := func(name string, fallback, low, high int) int {
		t.Helper()
		if raw := os.Getenv(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < low || n > high {
				t.Fatalf("%s must be in [%d,%d]", name, low, high)
			}
			return n
		}
		return fallback
	}
	count := setting("BP_WSSMUX_SOAK_USERS", 1000, 1, 25000)
	seconds := setting("BP_WSSMUX_SOAK_SECONDS", 600, 60, 7200)
	backend := startEchoBackend(t)
	tunnelPort, entryPort := freePort(t), freePort(t)
	const token = "wssmux-scale-soak-test-token-0123456789"
	srv := baseServerConfig("wssmux", tunnelPort, entryPort, backend.addr, token)
	cli := baseClientConfig("wssmux", fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil)
	// Match the observed pool/frame/buffer shape, without production secrets.
	srv.MuxCon, cli.ConnectionPool = 512, 64
	srv.ChannelSize = 8192
	srv.MuxVersion, cli.MuxVersion = 2, 2
	srv.MaxFrameSize, cli.MaxFrameSize = 65535, 65535
	srv.MaxReceiveBuffer, cli.MaxReceiveBuffer = 32<<20, 32<<20
	srv.MaxStreamBuffer, cli.MaxStreamBuffer = 32<<20, 32<<20
	srv.Nodelay, cli.Nodelay = true, true
	cli.AggressivePool = false
	tun := runPair(t, srv, cli, entryPort, tunnelPort)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatal(err)
	}
	connections := resourceIdleConnections(t, tun, count)
	runtime.GC()
	var initial runtime.MemStats
	runtime.ReadMemStats(&initial)
	t.Logf("SCALE_READY users=%d goroutines=%d fd=%d heap_bytes=%d", count, runtime.NumGoroutine(), countOpenFiles(), initial.HeapAlloc)

	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	var completed atomic.Int64
	errors := make(chan error, 8)
	payload := randomPayload(t, 4096)
	for range 8 {
		workers.Go(func() {
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if err := tun.roundTrip(payload); err != nil {
						errors <- err
						return
					}
					completed.Add(1)
				}
			}
		})
	}
	stop := sync.OnceFunc(func() { cancel(); workers.Wait() })
	defer stop()
	total := time.Duration(seconds) * time.Second
	started := time.Now()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	deadline := time.NewTimer(total)
	defer deadline.Stop()
	var samples []sample
sampling:
	for {
		select {
		case err := <-errors:
			t.Fatalf("verified churn failed after %d transfers: %v", completed.Load(), err)
		case <-deadline.C:
			break sampling
		case <-tick.C:
			s := takeSample()
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)
			t.Logf("SCALE_SAMPLE elapsed_s=%.0f completed=%d goroutines=%d fd=%d heap_bytes=%d total_alloc_bytes=%d gc=%d", time.Since(started).Seconds(), completed.Load(), s.goroutines, s.openFiles, mem.HeapAlloc, mem.TotalAlloc-initial.TotalAlloc, mem.NumGC-initial.NumGC)
			if time.Since(started) >= total/4 {
				samples = append(samples, s)
			}
		}
	}
	stop()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
	if completed.Load() == 0 || len(samples) < 6 {
		t.Fatal("insufficient traffic or samples")
	}
	third := len(samples) / 3
	early, late := samples[:third], samples[len(samples)-third:]
	checkNoSustainedRise(t, "WSSMux goroutines", meanInt(early, func(s sample) int { return s.goroutines }), meanInt(late, func(s sample) int { return s.goroutines }), 1.30)
	if early[0].openFiles > 0 {
		checkNoSustainedRise(t, "WSSMux descriptors", meanInt(early, func(s sample) int { return s.openFiles }), meanInt(late, func(s sample) int { return s.openFiles }), 1.30)
	}
	// Verify every quiet stream, not just an arbitrary survivor.
	for i, conn := range connections {
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		want := []byte("quiet stream survived WSSMux churn")
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("quiet stream %d: %v", i, err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("quiet stream %d corrupted or closed: %v", i, err)
		}
	}
	t.Logf("SCALE_DONE users=%d verified_churn=%d duration_s=%d quiet_users_verified=%d", count, completed.Load(), seconds, len(connections))
}
