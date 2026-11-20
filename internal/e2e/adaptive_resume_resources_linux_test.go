//go:build linux

package e2e

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Measure both managed peers, their TLS listeners, and the local echo fixture
// in one process. Use the same traffic and settings with resume off and on.
func TestManagedShortResumeResources(t *testing.T) {
	if os.Getenv("BP_RESUME_RESOURCE_BENCHMARK") != "1" {
		t.Skip("set BP_RESUME_RESOURCE_BENCHMARK=1 to measure")
	}
	payload := randomPayload(t, 256<<10)
	const streams = 4
	for i, enabled := range []bool{false, true, true, false} {
		t.Run(fmt.Sprintf("run%d_resume_%t", i+1, enabled), func(t *testing.T) {
			pair := startManagedShortPair(t, enabled)
			if err := pair.tunnel.roundTrip(payload[:64<<10]); err != nil {
				t.Fatal(err)
			}

			results := make(chan error, streams)
			var beforeHeap, afterHeap runtime.MemStats
			runtime.ReadMemStats(&beforeHeap)
			beforeCPU := managedShortProcessCPU(t)
			started := time.Now()
			for range streams {
				go func() { results <- pair.tunnel.roundTrip(payload) }()
			}
			for range streams {
				if err := <-results; err != nil {
					t.Error(err)
				}
			}
			elapsed := time.Since(started)
			cpu := managedShortProcessCPU(t) - beforeCPU
			runtime.ReadMemStats(&afterHeap)
			if t.Failed() {
				return
			}
			// Both directions are verified; count the echo payload once.
			verified := float64(streams * len(payload))
			t.Logf("RESUME_RESOURCE resume=%t streams=%d goodput_mbps=%.3f cpu_ms_per_mib=%.3f go_alloc_bytes_per_mib=%.0f",
				enabled, streams, verified*8/elapsed.Seconds()/1e6,
				cpu.Seconds()*1e3*(1<<20)/verified,
				float64(afterHeap.TotalAlloc-beforeHeap.TotalAlloc)*(1<<20)/verified)
			pair.tunnel.Stop()
			waitPortFree(t, pair.firstPort, 3*time.Second)
			waitPortFree(t, pair.secondPort, 3*time.Second)
		})
	}
}

func managedShortProcessCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano()+usage.Stime.Nano()) * time.Nanosecond
}
