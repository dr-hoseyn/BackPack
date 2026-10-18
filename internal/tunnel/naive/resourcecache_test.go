package naive

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHelperResourceCacheCoalescesConcurrentReaders(t *testing.T) {
	var c helperResourceCache
	var calls atomic.Int32
	now := time.Now()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			rss, cpu, ok := c.read(123, now, func() (uint64, float64, bool) {
				calls.Add(1)
				return 4096, 2.5, true
			})
			if !ok || rss != 4096 || cpu != 2.5 {
				t.Error("inconsistent shared resource sample")
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d process-tree scans for 32 simultaneous readers", calls.Load())
	}
}

func TestHelperResourceCacheExpiryRestartAndFailedSamples(t *testing.T) {
	var c helperResourceCache
	now := time.Now()
	calls := 0
	good := func() (uint64, float64, bool) { calls++; return uint64(calls), float64(calls), true }
	c.read(123, now, good)
	// A reader may capture its monotonic timestamp before another wins the lock.
	c.read(123, now.Add(-time.Millisecond), good)
	c.read(123, now.Add(4*time.Second), good)
	if calls != 1 {
		t.Fatal("did not reuse a fresh sample")
	}
	c.read(123, now.Add(5*time.Second), good)
	c.read(456, now.Add(5*time.Second), good)
	if calls != 3 {
		t.Fatal("expiry or helper replacement did not refresh")
	}
	if rss, cpu, ok := c.read(0, now, good); rss != 0 || cpu != 0 || ok {
		t.Fatal("stopped helper kept resources")
	}
	c.read(456, now, good)
	if calls != 4 {
		t.Fatal("stopped state did not invalidate the cache")
	}
	bad := func() (uint64, float64, bool) { calls++; return 999, 999, false }
	if rss, cpu, ok := c.read(789, now, bad); rss != 0 || cpu != 0 || ok {
		t.Fatal("partial sample escaped")
	}
	c.read(789, now.Add(500*time.Millisecond), good)
	if calls != 5 {
		t.Fatal("failed samples caused repeated scans")
	}
	c.read(789, now.Add(time.Second), good)
	if calls != 6 {
		t.Fatal("failed sample was not retried promptly")
	}
}
