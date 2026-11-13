package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func testWSSMuxDemand(ctx context.Context, queueSize int) *wssMuxDemand {
	return &wssMuxDemand{ctx: ctx, mu: new(sync.Mutex), streams: new(int32), sessions: new(int32), muxCon: 512, requests: make(chan struct{}, queueSize)}
}

func TestWSSMuxColdDemandCountsPhysicalCapacity(t *testing.T) {
	d := testWSSMuxDemand(t.Context(), 8192)
	for range 4000 {
		d.queued()
	}
	if got := len(d.requests); got != 11 {
		t.Fatalf("4000 users requested %d physical sessions, want 11 including reserve", got)
	}
	if got := atomic.LoadInt32(d.streams); got != 4000 {
		t.Fatalf("stream accounting changed: %d", got)
	}
	// Initial pool sessions may arrive before requested sessions. Once live
	// capacity suffices, obsolete requests must not launch more handshakes.
	atomic.StoreInt32(d.sessions, 1)
	d.arrived()
	if len(d.requests) != 10 || d.pending != 10 {
		t.Fatal("initial arrival did not replace an unsent capacity request")
	}
	atomic.StoreInt32(d.sessions, 64)
	d.arrived()
	if len(d.requests) != 0 || d.pending != 0 {
		t.Fatal("a warm initial pool retained obsolete demand")
	}
}

func TestWSSMuxDemandProgressesThroughAFullControlQueue(t *testing.T) {
	d := testWSSMuxDemand(t.Context(), 1)
	for range 4000 {
		d.queued()
	}
	for range 11 {
		select {
		case <-d.requests:
		default:
			t.Fatal("control queue saturation lost remaining capacity demand")
		}
		atomic.AddInt32(d.sessions, 1)
		d.arrived()
	}
	if len(d.requests) != 0 {
		t.Fatal("requested capacity beyond the existing reserve policy")
	}
}

func TestWSSMuxDemandRetriesLossWithoutAnAbsentClientStorm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		d := testWSSMuxDemand(ctx, 8192)
		done := make(chan struct{})
		go func() { defer close(done); d.run() }()
		for range 4000 {
			d.queued()
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if got := len(d.requests); got != 11 {
			t.Fatalf("absent control accumulated %d requests, want 11", got)
		}
		for range 11 {
			<-d.requests // consumed control requests, but the dials failed
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if len(d.requests) != 11 {
			t.Fatal("lost requests were not retried without another user")
		}
		atomic.StoreInt32(d.sessions, 11)
		d.arrived()
		if len(d.requests) != 0 {
			t.Fatal("successful recovery retained obsolete control requests")
		}
		atomic.StoreInt32(d.sessions, 0)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if len(d.requests) != 11 {
			t.Fatal("lost live capacity was not repaired")
		}
		cancel()
		<-done
		atomic.StoreInt32(d.streams, 0) // replacement generation's counter
		d.queued()
		d.arrived()
		if atomic.LoadInt32(d.streams) != 0 {
			t.Fatal("ended generation changed its replacement's counters")
		}
	})
}

func BenchmarkWSSMuxDemandSteadyAdmission(b *testing.B) {
	for _, coalesced := range []bool{false, true} {
		name := "baseline"
		if coalesced {
			name = "candidate"
		}
		b.Run(name, func(b *testing.B) {
			d := testWSSMuxDemand(b.Context(), 8192)
			atomic.StoreInt32(d.sessions, 64)
			atomic.StoreInt32(d.streams, 4000)
			queued := muxRequestForGeneration(d.ctx, d.mu, d.streams, d.sessions, d.muxCon, d.requests, quietLogger())
			if coalesced {
				queued = d.queued
			}
			b.ReportAllocs()
			for b.Loop() {
				queued()
				atomic.AddInt32(d.streams, -1)
			}
		})
	}
}
