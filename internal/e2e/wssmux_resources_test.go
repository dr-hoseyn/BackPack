package e2e

import (
	"bytes"
	"fmt"
	"io"
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
	if os.Getenv("BP_RESOURCE_NETEM") != "" {
		// Called only inside the disposable network namespace in CI. Match
		// the tunnel port so the application/backend hops stay local.
		commands := [][]string{
			{"qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio"},
			{"qdisc", "add", "dev", "lo", "parent", "1:3", "handle", "30:", "netem", "delay", "40ms", "loss", "0.1%", "rate", "200mbit", "limit", "10000"},
		}
		for _, direction := range []string{"dport", "sport"} {
			commands = append(commands, []string{"filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:", "prio", "1", "u32",
				"match", "ip", "protocol", "6", "0xff", "match", "ip", direction, strconv.Itoa(tunnelPort), "0xffff", "flowid", "1:3"})
		}
		for _, args := range commands {
			if output, err := exec.Command("tc", args...).CombinedOutput(); err != nil {
				t.Fatalf("netem setup: %v: %s", err, output)
			}
		}
		t.Cleanup(func() { _ = exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run() })
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
	started := time.Now()
	const workers = 8
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
	probes.Go(func() {
		for range 100 {
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
				if err := tun.roundTrip(payload); err != nil {
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
	cpuAfter, err := self.Times()
	if err != nil {
		t.Fatal(err)
	}
	stopCPU()
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
	connections := resourceIdleConnections(t, tun, 256)
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
	if peak <= 4 {
		t.Fatalf("fixture did not grow the pool: %d", peak)
	}
	deadline := time.Now().Add(90 * time.Second)
	for sessions() > 4 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if got := sessions(); got != 4 {
		t.Fatalf("pool retained %d sessions, want configured floor 4 (peak %d)", got, peak)
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
	t.Logf("RESOURCE_RETIREMENT peak_sessions=%d final_sessions=4 quiet_user=ok", peak)
}
