package e2e

import (
	"bytes"
	"context"
	"fmt"
	"github.com/BurntSushi/toml"
	"golang.org/x/time/rate"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"sync/atomic"
)

// This fixture intentionally uses only public engine configuration so exactly
// the same measurement can be copied to and run on the previous release.
func resourceTunnel(t *testing.T) (*tunnel, int) {
	t.Helper()
	backend := startEchoBackend(t)
	tunnelPort, entryPort := freePort(t), freePort(t)
	const token = "resource-profile-token-0123456789"
	srv := baseServerConfig("wssmux", tunnelPort, entryPort, backend.addr, token)
	cli := baseClientConfig("wssmux", fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil)
	srv.MuxCon, cli.ConnectionPool = 32, 4
	srv.ChannelSize = 4096
	// Presets already pin version 2. Keep the same protocol on both revisions
	// so this comparison isolates resource policy from protocol negotiation.
	srv.MuxVersion, cli.MuxVersion = 2, 2
	if os.Getenv("BP_RESOURCE_MUX_AUTO") != "" {
		srv.MuxVersion, cli.MuxVersion = 0, 0
	}
	srv.MaxReceiveBuffer, cli.MaxReceiveBuffer = 16<<20, 16<<20
	srv.MaxStreamBuffer, cli.MaxStreamBuffer = 8<<20, 8<<20
	srv.Nodelay, cli.Nodelay = true, true
	if os.Getenv("BP_WSSMUX_BOUNDED") != "" {
		// This fixture is also copied verbatim to the baseline release.
		// Decode the new optional cap rather than referring to its Go field.
		srv.MuxVersion, cli.MuxVersion = 2, 2
		srv.MuxCon, cli.ConnectionPool = 128, 8
		srv.MaxFrameSize, cli.MaxFrameSize = 32768, 32768
		srv.MaxReceiveBuffer, cli.MaxReceiveBuffer = 32<<20, 32<<20
		srv.MaxStreamBuffer, cli.MaxStreamBuffer = 16<<20, 16<<20
		cli.AggressivePool = false
		if _, err := toml.Decode("wsmux_max_sessions = 16", cli); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("BP_RESOURCE_NETEM") != "" {
		// Use receiver ingress, as tc-netem requires for realistic TCP/TSQ
		// results. MTU and offload settings make loss apply to wire-sized
		// packets instead of giant loopback/GSO packets. Everything below is
		// confined to a disposable network namespace in CI.
		commands := [][]string{
			{"ip", "link", "set", "lo", "mtu", "1500"},
			{"ip", "link", "add", "bpifb", "type", "ifb"},
			{"ip", "link", "set", "bpifb", "mtu", "1500", "up"},
			{"ethtool", "-K", "lo", "tso", "off", "gso", "off", "gro", "off"},
			{"ethtool", "-K", "bpifb", "tso", "off", "gso", "off", "gro", "off"},
			{"tc", "qdisc", "add", "dev", "bpifb", "root", "netem", "delay", "40ms", "loss", "0.1%", "rate", "200mbit", "limit", "10000", "seed", "42"},
			{"tc", "qdisc", "add", "dev", "lo", "handle", "ffff:", "ingress"},
		}
		for _, direction := range []string{"dport", "sport"} {
			commands = append(commands, []string{"tc", "filter", "add", "dev", "lo", "protocol", "ip", "parent", "ffff:", "prio", "1", "u32",
				"match", "ip", "protocol", "6", "0xff", "match", "ip", direction, strconv.Itoa(tunnelPort), "0xffff", "action", "mirred", "egress", "redirect", "dev", "bpifb"})
		}
		for _, args := range commands {
			if output, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
				t.Fatalf("netem setup: %v: %s", err, output)
			}
		}
		t.Cleanup(func() {
			_ = exec.Command("tc", "qdisc", "del", "dev", "lo", "ingress").Run()
			_ = exec.Command("ip", "link", "del", "bpifb").Run()
		})
	}
	tun := runPair(t, srv, cli, entryPort, tunnelPort)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatal(err)
	}
	return tun, tunnelPort
}

func resourceIdleConnections(t *testing.T, tun *tunnel, count int) []net.Conn {
	t.Helper()
	connections := make([]net.Conn, 0, count)
	t.Cleanup(func() {
		for _, c := range connections {
			c.Close()
		}
	})
	for range count {
		c, err := net.DialTimeout("tcp", tun.Entry, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, c)
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err = c.Write([]byte{0x61}); err != nil {
			t.Fatal(err)
		}
		var got [1]byte
		if _, err = io.ReadFull(c, got[:]); err != nil || got[0] != 0x61 {
			t.Fatalf("idle stream readiness: %x, %v", got, err)
		}
		_ = c.SetDeadline(time.Time{})
	}
	return connections
}

func TestWSMuxResourceProfile(t *testing.T) {
	if os.Getenv("BP_RESOURCE_BENCHMARK") == "" {
		t.Skip("set BP_RESOURCE_BENCHMARK=1")
	}
	count := 1000
	if value := os.Getenv("BP_RESOURCE_IDLE_CONNECTIONS"); value != "" {
		var err error
		count, err = strconv.Atoi(value)
		if err != nil || count < 1 || count > 2000 {
			t.Fatal("invalid idle connection count")
		}
	}
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	tun, _ := resourceTunnel(t)
	runtime.GC()
	var before, idle, after runtime.MemStats
	runtime.ReadMemStats(&before)
	connections := resourceIdleConnections(t, tun, count)
	runtime.GC()
	runtime.ReadMemStats(&idle)
	mem, err := self.MemoryInfo()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("RESOURCE_IDLE connections=%d heap_delta_bytes=%d heap_bytes=%d rss_bytes=%d goroutines=%d", count,
		int64(idle.HeapAlloc)-int64(before.HeapAlloc), idle.HeapAlloc, mem.RSS, runtime.NumGoroutine())
	profileDir := os.Getenv("BP_RESOURCE_PROFILE_DIR")
	profileName := os.Getenv("BP_RESOURCE_PROFILE_NAME")
	if profileDir != "" {
		f, err := os.Create(filepath.Join(profileDir, "resource-"+profileName+"-heap.pprof"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.WriteHeapProfile(f); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	payload := randomPayload(t, 4<<20)
	if err := tun.roundTrip(payload); err != nil {
		t.Fatal(err)
	}
	stopCPU := func() {}
	if profileDir != "" {
		f, err := os.Create(filepath.Join(profileDir, "resource-"+profileName+"-cpu.pprof"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			t.Fatal(err)
		}
		stopCPU = sync.OnceFunc(func() { pprof.StopCPUProfile(); f.Close() })
		defer stopCPU()
	}
	cpuBefore, err := self.Times()
	if err != nil {
		t.Fatal(err)
	}
	var peakRSS atomic.Uint64
	peakRSS.Store(mem.RSS)
	stopSample, sampleDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampleDone)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopSample:
				return
			case <-tick.C:
				if m, e := self.MemoryInfo(); e == nil {
					for old := peakRSS.Load(); m.RSS > old; old = peakRSS.Load() {
						if peakRSS.CompareAndSwap(old, m.RSS) {
							break
						}
					}
				}
			}
		}
	}()
	stopSampling := sync.OnceFunc(func() { close(stopSample); <-sampleDone })
	defer stopSampling()
	started := time.Now()
	const workers = 8
	var offered *rate.Limiter
	if raw := os.Getenv("BP_RESOURCE_TARGET_MBPS"); raw != "" {
		target, e := strconv.ParseFloat(raw, 64)
		if e != nil || math.IsNaN(target) || math.IsInf(target, 0) || target <= 0 || target > 10000 {
			t.Fatal("invalid resource target Mbps")
		}
		offered = rate.NewLimiter(rate.Limit(target*1e6/8), 64<<10)
		t.Logf("RESOURCE_TARGET application_mbps=%.1f", target)
	}
	rounds := 16
	if value := os.Getenv("BP_RESOURCE_ROUNDS"); value != "" {
		rounds, err = strconv.Atoi(value)
		if err != nil || rounds < 1 || rounds > 64 {
			t.Fatal("invalid resource transfer rounds")
		}
	}
	results := make(chan error, workers)
	var probes sync.WaitGroup
	var latencies []time.Duration
	var probeErr error
	bulkDone := make(chan struct{})
	finishBulk := sync.OnceFunc(func() { close(bulkDone) })
	defer finishBulk()
	probes.Go(func() {
		for probe := 0; ; probe++ {
			if probe >= 100 {
				select {
				case <-bulkDone:
					return
				default:
				}
			}
			start := time.Now()
			if err := tun.roundTrip(payload[:128]); err != nil {
				probeErr = err
				return
			}
			latencies = append(latencies, time.Since(start))
			time.Sleep(5 * time.Millisecond)
		}
	})
	for range workers {
		go func() {
			for range rounds {
				if err := resourceOfferedRoundTrip(tun, payload, offered); err != nil {
					results <- err
					return
				}
			}
			results <- nil
		}()
	}
	for range workers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	seconds := time.Since(started).Seconds()
	finishBulk()
	cpuAfter, err := self.Times()
	if err != nil {
		t.Fatal(err)
	}
	stopCPU()
	stopSampling()
	t.Logf("RESOURCE_PEAK sampled_peak_rss_bytes=%d", peakRSS.Load())
	probes.Wait()
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	runtime.ReadMemStats(&after)
	appBytes := float64(workers * rounds * len(payload))
	cpu := cpuAfter.Total() - cpuBefore.Total()
	t.Logf("RESOURCE_MIXED app_bytes=%.0f echo_mbps=%.1f cpu_seconds=%.6f cpu_seconds_per_gib=%.6f probe_p99_ms=%.3f gc_cycles=%d gc_pause_ms=%.3f", appBytes,
		appBytes*8/seconds/1e6, cpu, cpu/(appBytes/(1<<30)), float64(latencies[(len(latencies)*99+99)/100-1])/float64(time.Millisecond), after.NumGC-idle.NumGC,
		float64(after.PauseTotalNs-idle.PauseTotalNs)/1e6)
	for _, c := range connections {
		c.Close()
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("RESOURCE_AFTER heap_bytes=%d goroutines=%d", after.HeapAlloc, runtime.NumGoroutine())
}

func TestWSMuxPoolRetiresAfterLoadWithoutDisconnectingQuietUser(t *testing.T) {
	if os.Getenv("BP_RESOURCE_RETIREMENT") == "" {
		t.Skip("set BP_RESOURCE_RETIREMENT=1 for real 60-second retirement")
	}
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	tun, port := resourceTunnel(t)
	floor, idleCount := 4, 256
	if os.Getenv("BP_WSSMUX_BOUNDED") != "" {
		floor, idleCount = 8, 1152
	}
	connections := resourceIdleConnections(t, tun, idleCount)
	for _, c := range connections[1:] {
		c.Close()
	}
	sessions := func() int {
		rows, err := self.Connections()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, row := range rows {
			if row.Raddr.Port == uint32(port) && row.Status == "ESTABLISHED" {
				n++
			}
		}
		return n - 1 // exclude the control channel
	}
	peak := sessions()
	if peak <= floor {
		t.Fatalf("fixture did not grow the pool: %d", peak)
	}
	deadline := time.Now().Add(120 * time.Second)
	for sessions() > floor && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if got := sessions(); got != floor {
		t.Fatalf("pool retained %d sessions, want configured floor %d (peak %d)", got, floor, peak)
	}
	// The remaining stream has been silent longer than the retirement period.
	c := connections[0]
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	want := []byte("quiet user survived pool retirement")
	if _, err := c.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("quiet user: %q, %v", got, err)
	}
	t.Logf("RESOURCE_RETIREMENT peak_sessions=%d final_sessions=%d quiet_user=ok", peak, floor)
}

// Shared offered-load pacing applies to small writes, not entire 4 MB bursts.
// Readers remain concurrent so flow control and echo backpressure are real.
func resourceOfferedRoundTrip(tun *tunnel, payload []byte, limiter *rate.Limiter) error {
	if limiter == nil {
		return tun.roundTrip(payload)
	}
	conn, err := net.DialTimeout("tcp", tun.Entry, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	written := make(chan error, 1)
	go func() {
		for offset := 0; offset < len(payload); {
			end := min(offset+32*1024, len(payload))
			if e := limiter.WaitN(ctx, end-offset); e != nil {
				written <- e
				return
			}
			for offset < end {
				n, e := conn.Write(payload[offset:end])
				offset += n
				if e != nil {
					written <- e
					return
				}
				if n == 0 {
					written <- io.ErrShortWrite
					return
				}
			}
		}
		written <- nil
	}()
	got := make([]byte, len(payload))
	_, err = io.ReadFull(conn, got)
	if err != nil {
		cancel()
		conn.Close()
	}
	if writeErr := <-written; err == nil {
		err = writeErr
	}
	if err == nil && !bytes.Equal(got, payload) {
		return fmt.Errorf("paced transfer corrupted")
	}
	return err
}
