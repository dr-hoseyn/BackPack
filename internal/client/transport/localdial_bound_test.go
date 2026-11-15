package transport

import (
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDialFailureReportingDoesNotRetainEveryPastDestination(t *testing.T) {
	logger, _ := countingLogger()
	r := &localDialReporter{last: map[string]time.Time{}}
	for i := 0; i < 4096; i++ {
		r.Report(logger, fmt.Sprintf("backend-%d:8443", i), syscall.ECONNREFUSED)
	}
	if len(r.last) > 1024 {
		t.Fatalf("dial failure reporter retained %d past destinations, want at most 1024", len(r.last))
	}
}

func TestConcurrentFailedDestinationsKeepTheReporterBounded(t *testing.T) {
	logger, _ := countingLogger()
	r := &localDialReporter{last: map[string]time.Time{}}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Go(func() {
			for i := 0; i < 64; i++ {
				r.Report(logger, fmt.Sprintf("backend-%d-%d:8443", worker, i), syscall.ECONNREFUSED)
			}
		})
	}
	workers.Wait()
	if len(r.last) > localDialMaxAddresses {
		t.Fatalf("concurrent failures retained %d destinations, want at most %d", len(r.last), localDialMaxAddresses)
	}
}

func TestDialFailureCachePressureKeepsRecentRepeatSuppression(t *testing.T) {
	logger, count := countingLogger()
	r := &localDialReporter{last: map[string]time.Time{}}
	for i := 0; i < 1024; i++ {
		r.last[fmt.Sprintf("backend-%d:8443", i)] = time.Now()
	}
	r.last["backend-0:8443"] = time.Now().Add(-localDialQuiet / 2)
	r.Report(logger, "new-backend:8443", syscall.ECONNREFUSED)
	if _, exists := r.last["backend-0:8443"]; exists {
		t.Error("oldest quiet-period entry was retained under cache pressure")
	}
	r.Report(logger, "backend-1023:8443", syscall.ECONNREFUSED)
	r.Report(logger, "new-backend:8443", syscall.ECONNREFUSED)
	if count() != 1 {
		t.Fatalf("recent repeated failures escaped suppression: %d log lines, want 1", count())
	}
}

func TestDialFailureCacheReclaimsExpiredAddressesBeforeRecentOnes(t *testing.T) {
	logger, count := countingLogger()
	r := &localDialReporter{last: map[string]time.Time{}}
	for i := 0; i < 1023; i++ {
		r.last[fmt.Sprintf("expired-%d:8443", i)] = time.Now().Add(-2 * localDialQuiet)
	}
	r.last["recent:8443"] = time.Now()
	r.Report(logger, "new:8443", syscall.ECONNREFUSED)
	if len(r.last) != 2 {
		t.Errorf("cache retains %d destinations after expiry, want 2", len(r.last))
	}
	r.Report(logger, "recent:8443", syscall.ECONNREFUSED)
	if count() != 1 {
		t.Error("reclaiming expired addresses lost suppression of a recent failure")
	}
}
