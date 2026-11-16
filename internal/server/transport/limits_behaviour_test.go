package transport

import (
	"sync"
	"testing"
	"time"
)

// The connection limiter, tested by what it does rather than by what its
// callers look like.
//
// limitrelease_test.go and pairingtimeout_test.go both read the source of the
// transports and assert that a `release()` appears on a particular branch.
// That is a reasonable way to check six near-identical copies at once, and it
// is not a test of the limiter: it would pass unchanged if acquire() stopped
// counting, if release() went negative, or if two goroutines could both take
// the last slot.
//
// This package is the least covered in the tree — 12.6% across 9,103 lines —
// and the limiter is the part of it that has already leaked in production. So
// it gets tested directly.

func TestAnUnlimitedTunnelPaysNothing(t *testing.T) {
	// The zero value has to enforce nothing, because most tunnels configure no
	// limits at all and the unlimited path is the hot one.
	if l := newLimiter(Limits{}); l != nil {
		t.Errorf("newLimiter(no limits) = %v, want nil so callers pay only a nil check", l)
	}

	// And a nil limiter has to survive every method, because that is exactly
	// what the transports call it with.
	var nilLim *limiter
	if !nilLim.acquire() {
		t.Error("a nil limiter refused a connection; unlimited must mean unlimited")
	}
	nilLim.release() // must not panic

}

func TestTheConnectionCapIsExactAndSlotsComeBack(t *testing.T) {
	const cap = 3
	l := newLimiter(Limits{MaxConnections: cap})
	if l == nil {
		t.Fatal("newLimiter returned nil for a tunnel that has a connection cap")
	}

	for i := 0; i < cap; i++ {
		if !l.acquire() {
			t.Fatalf("slot %d of %d was refused", i+1, cap)
		}
	}
	// One past the cap.
	if l.acquire() {
		t.Fatalf("a %dth connection was admitted on a cap of %d", cap+1, cap)
	}
	// A refused acquire must not consume anything: this is the leak shape —
	// the count went up and was never brought back down, so the tunnel
	// eventually refused everything with a limit that looked right in the
	// config.
	l.release()
	if !l.acquire() {
		t.Error("after one release a slot was still refused; a refused acquire is " +
			"holding a slot it never got")
	}

	// Releasing everything returns to the starting state rather than drifting
	// negative, which would let the tunnel exceed its own cap later.
	for i := 0; i < cap; i++ {
		l.release()
	}
	for i := 0; i < cap; i++ {
		if !l.acquire() {
			t.Fatalf("after a full release cycle, slot %d was refused", i+1)
		}
	}
	if l.acquire() {
		t.Error("the cap drifted: more than the configured number of slots are available")
	}
}

// The cap has to hold when the connections arrive at once, which is the only
// way they actually arrive. Run under -race this also covers the counter
// itself.
func TestTheConnectionCapHoldsUnderConcurrentAccepts(t *testing.T) {
	const (
		cap     = 8
		callers = 200
	)
	l := newLimiter(Limits{MaxConnections: cap})

	var (
		mu       sync.Mutex
		admitted int
		peak     int
		wg       sync.WaitGroup
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !l.acquire() {
				return
			}
			mu.Lock()
			admitted++
			if admitted > peak {
				peak = admitted
			}
			mu.Unlock()

			time.Sleep(time.Millisecond) // hold the slot briefly

			mu.Lock()
			admitted--
			mu.Unlock()
			l.release()
		}()
	}
	wg.Wait()

	if peak > cap {
		t.Errorf("%d connections were live at once on a cap of %d", peak, cap)
	}
	if admitted != 0 {
		t.Errorf("%d slots are still marked in use after every caller finished", admitted)
	}
	// And the limiter agrees it is empty.
	for i := 0; i < cap; i++ {
		if !l.acquire() {
			t.Fatalf("the limiter leaked: slot %d unavailable after everything released", i+1)
		}
	}
}
