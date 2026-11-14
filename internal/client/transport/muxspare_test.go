package transport

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/sirupsen/logrus"
)

func TestWSMuxSpareGuardRequiresSustainedKnownHealthyReserve(t *testing.T) {
	now := time.Unix(100, 0)
	healthy := muxPoolSample{active: 4, total: 12, streams: 500, pressureObserved: 12}
	for _, tc := range []struct {
		name        string
		enabled     bool
		sample      muxPoolSample
		live, floor int
	}{
		{"disabled preserves growth", false, healthy, 12, 8},
		{"missing socket observation", true, muxPoolSample{active: 4, total: 12, pressureObserved: 11}, 12, 8},
		{"pressure on an empty carrier", true, muxPoolSample{active: 4, total: 12, pressureObserved: 12, pressured: 1}, 12, 8},
		{"not enough empty capacity", true, muxPoolSample{active: 5, total: 12, pressureObserved: 12}, 12, 8},
		{"dial or teardown in flight", true, healthy, 13, 8},
		{"empty pool", true, muxPoolSample{}, 0, 8},
		{"invalid floor", true, healthy, 12, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var g muxSpareGuard
			for _, elapsed := range []time.Duration{0, time.Minute} {
				if !g.allowThroughputGrowth(now.Add(elapsed), tc.enabled, tc.sample, tc.live, tc.floor) {
					t.Fatal("unknown, busy or disabled state suppressed throughput growth")
				}
			}
		})
	}
	var g muxSpareGuard
	for _, elapsed := range []time.Duration{0, 29 * time.Second} {
		if !g.allowThroughputGrowth(now.Add(elapsed), true, healthy, 12, 8) {
			t.Fatal("growth deferred before the observation period")
		}
	}
	if g.allowThroughputGrowth(now.Add(30*time.Second), true, healthy, 12, 8) {
		t.Fatal("known healthy spare capacity did not defer throughput growth")
	}
	unknown := healthy
	unknown.pressureObserved--
	if !g.allowThroughputGrowth(now.Add(31*time.Second), true, unknown, 12, 8) {
		t.Fatal("lost visibility did not restore growth immediately")
	}
	if !g.allowThroughputGrowth(now.Add(32*time.Second), true, healthy, 12, 8) {
		t.Fatal("a transient unknown sample did not reset hysteresis")
	}
}

func TestWSMuxSpareGuardNeverRemovesDemandGrowth(t *testing.T) {
	// Run the actual sizing loop with real smux sessions. Virtual time lets the
	// healthy reserve mature before a high byte-rate sample and a demand burst.
	synctest.Test(t, func(t *testing.T) {
		metrics.ClearPool()
		t.Cleanup(metrics.ClearPool)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var sessions muxPoolSessions
		for i := range 12 {
			session, _ := diagnosticMuxPair(t)
			remove := sessions.addObserved(session, true, func() (int, bool) { return 0, true })
			t.Cleanup(remove)
			if i < 4 {
				stream, err := session.OpenStream()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { stream.Close() })
			}
		}
		var open, pending, taken, dials int32
		open = 12
		log := logrus.New()
		log.SetOutput(io.Discard)
		done := make(chan struct{})
		go func() {
			defer close(done)
			poolSizer{ctx: ctx, log: log, size: 8, maxSize: 64, mux: true,
				open: &open, pending: &pending, taken: &taken, sessions: &sessions,
				poolDiagnostics: true, conservativeMux: true,
				spawn: func(fn func()) { fn() }, dial: func() { atomic.AddInt32(&dials, 1) },
			}.maintain()
		}()
		synctest.Wait()
		time.Sleep(31 * time.Second)
		synctest.Wait()
		metrics.AddBytes(64<<20, 0)
		time.Sleep(time.Second)
		synctest.Wait()
		p := (&metrics.Collector{}).Snapshot().Pool
		if p == nil || p.Target != 8 || p.Mux == nil || !p.Mux.ThroughputGrowthSuppressed {
			t.Fatalf("healthy spare capacity did not defer the byte-rate signal: %+v", p)
		}
		atomic.StoreInt32(&taken, 1000)
		time.Sleep(time.Second)
		synctest.Wait()
		_, target, _, _ := metrics.PoolState()
		if target != 64 || atomic.LoadInt32(&dials) <= 8 {
			t.Fatalf("demand burst did not request new carriers: target %d, dials %d", target, atomic.LoadInt32(&dials))
		}
		cancel()
		<-done
	})
}
