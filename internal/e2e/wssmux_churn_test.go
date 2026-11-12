package e2e

import (
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// This same fixture can be copied to the parent revision for paired runs.
// It measures real authenticated WSSmux stream opens and byte-verified replies,
// including both tunnel peers and the test driver in one process.
func TestWSMuxConnectionChurnProfile(t *testing.T) {
	if os.Getenv("BP_WSSMUX_CHURN") == "" {
		t.Skip("set BP_WSSMUX_CHURN=1")
	}
	tun, _ := resourceTunnel(t)
	payload := randomPayload(t, 512)
	if err := tun.roundTrip(payload); err != nil {
		t.Fatal(err)
	}
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	before, err := self.Times()
	if err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 16, 256
	latencies := make(chan time.Duration, workers*rounds)
	errors := make(chan error, workers)
	var done sync.WaitGroup
	started := time.Now()
	for range workers {
		done.Go(func() {
			for range rounds {
				start := time.Now()
				if err := tun.roundTrip(payload); err != nil {
					errors <- err
					return
				}
				latencies <- time.Since(start)
			}
		})
	}
	done.Wait()
	elapsed := time.Since(started).Seconds()
	after, err := self.Times()
	if err != nil {
		t.Fatal(err)
	}
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	close(latencies)
	var samples []time.Duration
	for latency := range latencies {
		samples = append(samples, latency)
	}
	if len(samples) != workers*rounds {
		t.Fatal("incomplete verified churn workload")
	}
	slices.Sort(samples)
	cpu := after.User + after.System - before.User - before.System
	t.Logf("CHURN connections=%d app_bytes=%d elapsed_seconds=%.3f cpu_seconds=%.3f cpu_us_per_connection=%.3f p99_ms=%.3f",
		len(samples), len(samples)*len(payload), elapsed, cpu, cpu*1e6/float64(len(samples)),
		float64(samples[(len(samples)*99-1)/100])/float64(time.Millisecond))
}
