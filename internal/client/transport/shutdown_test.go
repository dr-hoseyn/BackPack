package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestCancelledClientJoinsPoolWorkersBeforeItsOwnerReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var l lifecycle
		l.firstGeneration(ctx, silentLogger(), usageSpec{})
		stopping := make(chan struct{})
		release := make(chan struct{})
		releaseWorker := sync.OnceFunc(func() { close(release) })
		defer releaseWorker()
		l.state.Go(func() {
			<-ctx.Done()
			close(stopping)
			<-release
		})
		done := make(chan struct{})
		go func() { l.WaitStopped(); close(done) }()
		cancel()
		<-stopping
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("client owner returned with an old pool worker still alive")
		default:
		}
		releaseWorker()
		<-done
		var late atomic.Bool
		l.state.Go(func() { late.Store(true) })
		synctest.Wait()
		if late.Load() {
			t.Fatal("joined generation admitted a late worker")
		}
	})
}

func TestClientShutdownJoinsAnAlreadyRunningReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var l lifecycle
	l.firstGeneration(ctx, silentLogger(), usageSpec{})
	inTeardown := make(chan struct{})
	release := make(chan struct{})
	releaseWorker := sync.OnceFunc(func() { close(release) })
	defer releaseWorker()
	restarted := make(chan struct{})
	var newGeneration atomic.Bool
	go func() {
		defer close(restarted)
		l.restart(func() { close(inTeardown); <-release }, nil, func() { newGeneration.Store(true) })
	}()
	<-inTeardown
	cancel()
	done := make(chan struct{})
	go func() { l.WaitStopped(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown returned while reconnect still owned the generation")
	case <-time.After(20 * time.Millisecond):
	}
	releaseWorker()
	<-done
	<-restarted
	if newGeneration.Load() {
		t.Fatal("cancelled reconnect created a replacement generation")
	}
}
