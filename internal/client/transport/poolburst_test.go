package transport

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestABurstGetsCapacityBeforeTheServerPairingDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var open, taken int32
	atomic.StoreInt32(&taken, 200)
	log := logrus.New()
	log.SetOutput(io.Discard)
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		poolSizer{
			ctx: ctx, log: log, size: 2, maxSize: 16, mux: true,
			open: &open, taken: &taken, shrink: make(chan struct{}, 1),
			spawn: func(fn func()) {
				workers.Add(1)
				go func() { defer workers.Done(); fn() }()
			},
			dial: func() {
				atomic.AddInt32(&open, 1)
				defer atomic.AddInt32(&open, -1)
				<-ctx.Done()
			},
		}.maintain()
	}()
	t.Cleanup(func() { cancel(); <-done; workers.Wait() })
	deadline := time.Now().Add(2500 * time.Millisecond)
	for atomic.LoadInt32(&open) < 16 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&open); got != 16 {
		t.Fatalf("burst left only %d sessions before users expire; want the bounded 16", got)
	}
}

func TestBurstGrowthAndThroughputRespectTheResourceCeiling(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		current, ceiling, requests, mbps int
		mux                              bool
		want                             int
	}{
		{"connection burst", 8, 32, 1000, 0, false, 32},
		{"physical sessions carrying 500 Mbit", 8, 64, 0, 500, true, 16},
		{"spare streams do not divide traffic", 8, 64, 0, 500, false, 12},
		{"memory ceiling", 8, 8, 1000, 500, true, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := poolGrowthTarget(tc.current, tc.ceiling, tc.requests, 5, tc.mbps, tc.mux); got != tc.want {
				t.Fatalf("target = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLargeReceiveWindowsDoNotDisableSessionGrowth(t *testing.T) {
	if got := muxPoolLimit(8); got < 64 {
		t.Fatalf("automatic capacity is still limited to a small receive-window budget: %d", got)
	}
	ordinary := &TcpTransport{config: &TcpConfig{ConnPoolSize: 32}}
	short := &TcpTransport{config: &TcpConfig{ConnPoolSize: 32, FixedPool: true}}
	if ordinary.poolLimit() <= 32 || short.poolLimit() != 32 {
		t.Fatal("ordinary growth must expand while the finite Short HTTPS table stays fixed")
	}
}

func TestAnUnavailableCarrierRepairsTheFloorWithoutADialStorm(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var open, taken, pending, attempts int32
	log := logrus.New()
	log.SetOutput(io.Discard)
	done := make(chan struct{})
	go func() {
		defer close(done)
		poolSizer{
			ctx: ctx, log: log, size: 2, maxSize: 64,
			open: &open, taken: &taken, pending: &pending,
			shrink: make(chan struct{}, 1),
			// Synchronous failed dials isolate the repair decision from
			// scheduler delays and do not leave a connection in the pool.
			spawn: func(fn func()) { fn() },
			dial:  func() { atomic.AddInt32(&attempts, 1) },
		}.maintain()
	}()
	time.Sleep(1500 * time.Millisecond)
	cancel()
	<-done
	if got := atomic.LoadInt32(&attempts); got < 2 || got > 4 {
		t.Fatalf("idle unavailable carrier made %d dials; want only the floor and its repair", got)
	}
}
