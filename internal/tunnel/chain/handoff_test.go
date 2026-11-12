package chain

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestReplacementWaitsForThePreviousCandidateToReleaseItsWorkers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := fast("first", "second")
		c.teardown = time.Hour // a joined candidate needs no guessed grace period
		stopping := make(chan struct{})
		release := make(chan struct{})
		releaseWorker := sync.OnceFunc(func() { close(release) })
		defer releaseWorker()
		stopped := make(chan struct{})
		second := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, true, func(owner context.Context, name string) Attempt {
				if name == "second" {
					close(second)
					return Attempt{Settled: func() bool { return true }}
				}
				go func() {
					<-owner.Done()
					close(stopping)
					<-release
					close(stopped)
				}()
				return Attempt{Settled: func() bool { return false }, WaitStopped: func() { <-stopped }}
			})
		}()
		<-stopping
		synctest.Wait()
		select {
		case <-second:
			t.Fatal("replacement started while previous workers were still held")
		default:
		}
		before := time.Now()
		releaseWorker()
		<-second
		if time.Since(before) >= time.Hour {
			t.Fatal("joined workers still incurred the fixed teardown delay")
		}
		cancel()
		<-done
	})
}

func TestChainShutdownJoinsEvenASingleCandidate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		stopping := make(chan struct{})
		release := make(chan struct{})
		releaseWorker := sync.OnceFunc(func() { close(release) })
		defer releaseWorker()
		done := make(chan struct{})
		go func() {
			defer close(done)
			fast("only").Run(ctx, true, func(owner context.Context, _ string) Attempt {
				close(started)
				return Attempt{WaitStopped: func() {
					if owner.Err() == nil {
						t.Error("join called before candidate cancellation")
					}
					close(stopping)
					<-release
				}}
			})
		}()
		<-started
		cancel()
		<-stopping
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("chain returned while candidate teardown was still held")
		default:
		}
		releaseWorker()
		<-done
	})
}
