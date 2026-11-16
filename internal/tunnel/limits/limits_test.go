package limits

import (
	"sync"
	"testing"
)

// A tunnel that asked for nothing must get a nil limiter, so the unlimited
// path stays free.
func TestUnlimitedIsNil(t *testing.T) {
	if l := New(Config{}); l != nil {
		t.Fatal("an unlimited config produced a limiter")
	}
	var nilLimiter *Limiter
	if !nilLimiter.Acquire() {
		t.Fatal("a nil limiter refused a connection")
	}
	nilLimiter.Release() // must not panic
	if nilLimiter.Active() != 0 {
		t.Fatal("a nil limiter reported active connections")
	}

}

func TestConnectionCap(t *testing.T) {
	l := New(Config{MaxConnections: 3})

	for i := 0; i < 3; i++ {
		if !l.Acquire() {
			t.Fatalf("slot %d was refused while under the cap", i)
		}
	}
	if l.Acquire() {
		t.Fatal("a fourth connection was admitted past a cap of three")
	}
	if l.Active() != 3 {
		t.Fatalf("Active = %d, want 3", l.Active())
	}

	l.Release()
	if !l.Acquire() {
		t.Fatal("a released slot was not reusable")
	}
}

// A refused Acquire must not leave a slot consumed, or the cap would erode
// with every refusal until nothing could connect at all.
func TestRefusalDoesNotLeakASlot(t *testing.T) {
	l := New(Config{MaxConnections: 1})
	if !l.Acquire() {
		t.Fatal("the first connection was refused")
	}
	for i := 0; i < 100; i++ {
		if l.Acquire() {
			t.Fatal("a second connection was admitted past a cap of one")
		}
	}
	l.Release()
	if !l.Acquire() {
		t.Fatal("the cap eroded: the slot could not be taken after 100 refusals")
	}
}

func TestConnectionCapUnderConcurrency(t *testing.T) {
	const cap = 10
	l := New(Config{MaxConnections: cap})

	var wg sync.WaitGroup
	admitted := make(chan struct{}, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Acquire() {
				admitted <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(admitted)

	if got := len(admitted); got != cap {
		t.Fatalf("%d connections were admitted past a cap of %d", got, cap)
	}
}
