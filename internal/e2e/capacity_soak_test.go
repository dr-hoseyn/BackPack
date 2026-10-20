package e2e

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Sustained churn exposes a pool that works after restart and loses admission
// capacity once its first sessions fill. Probe small fresh flows throughout
// concurrent transfers and verify that shutdown returns the resources.
func TestBulkProfileKeepsAcceptingUsersUnderSustainedLoad(t *testing.T) {
	seconds, _ := strconv.Atoi(os.Getenv("BP_CAPACITY_SOAK_SECONDS"))
	if seconds == 0 {
		t.Skip("set BP_CAPACITY_SOAK_SECONDS=60 or more")
	}
	if seconds < 60 {
		t.Fatal("a sustained capacity check needs at least 60 seconds")
	}
	for _, tr := range []string{"tcpmux", "wssmux", "kcp", "quic"} {
		t.Run(tr, func(t *testing.T) {
			before := openFDs(t)
			tun := bulkProfileTunnel(t, tr)
			payload := randomPayload(t, 128<<10)
			stop := make(chan struct{})
			var workers sync.WaitGroup
			var completed, failed atomic.Int64
			for i := 0; i < 8; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						if err := tun.roundTrip(payload); err != nil {
							failed.Add(1)
						} else {
							completed.Add(1)
						}
					}
				}()
			}
			deadline := time.Now().Add(time.Duration(seconds) * time.Second)
			var probes, probeFailures int
			for time.Now().Before(deadline) {
				probes++
				if err := tun.roundTrip([]byte("fresh-user-under-load")); err != nil {
					probeFailures++
				}
				time.Sleep(250 * time.Millisecond)
			}
			close(stop)
			workers.Wait()
			tun.Stop()
			time.Sleep(time.Second)
			t.Logf("SOAK transport=%s seconds=%d transfers=%d failed=%d probes=%d probe_failures=%d", tr, seconds, completed.Load(), failed.Load(), probes, probeFailures)
			if completed.Load() == 0 || failed.Load() != 0 || probeFailures != 0 {
				t.Fatal("bulk traffic or fresh users failed under sustained load")
			}
			if after := openFDs(t); before > 0 && after > before+64 {
				t.Fatalf("capacity retained descriptors after shutdown: before=%d after=%d", before, after)
			}
		})
	}
}
