package transport

import (
	"fmt"
	"testing"
)

func TestWSMuxReadyOrderMatchesFullCongestionOrdering(t *testing.T) {
	// Exhaust six carriers at four pressure levels. Occupancy/recency must
	// not let a newer healthy waiter jump an older healthy waiter.
	levels := [...]int{0, 1, 8, 64}
	for state := range 4096 {
		q := newBalancedMuxLocalQueue(1)
		var expected *muxQueueWaiter
		bestPressure, firstHealthy, queries := 0, -1, 0
		for i := range 6 {
			pressure := levels[(state>>(2*i))&3]
			w := wsMuxWaiter((state+7*i)%16, func() int { queries++; return pressure })
			w.lastUse = uint64(6 - i)
			q.wait(w)
			if firstHealthy < 0 && pressure == 0 {
				firstHealthy = i
			}
			if expected == nil || pressure < bestPressure {
				expected, bestPressure = w, pressure
			}
		}
		q.push(LocalTCPConn{timeCreated: 1}, nil)
		if q.selected != expected {
			t.Fatalf("state %d changed congestion/readiness order", state)
		}
		expectedQueries := 6
		if firstHealthy >= 0 {
			expectedQueries = firstHealthy + 1
		}
		if queries != expectedQueries {
			t.Fatalf("state %d sampled %d carriers, want %d", state, queries, expectedQueries)
		}
	}
}

func TestWSMuxQuietUsersDoNotDisplaceAnOlderHealthyWaiter(t *testing.T) {
	q := newBalancedMuxLocalQueue(2)
	older, newer := wsMuxWaiter(100, nil), wsMuxWaiter(0, nil)
	q.wait(older)
	q.wait(newer)
	q.wait(older) // redundant registration must not move it to the tail
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	takeScheduledWSMux(t, q, older, 1)
	q.wait(older) // after serving, return behind the already waiting peer
	q.push(LocalTCPConn{timeCreated: 2}, nil)
	takeScheduledWSMux(t, q, newer, 2)
	q.leave(older)
	q.leave(newer)
	if q.head != nil || q.tail != nil {
		t.Fatal("departed carriers retained by readiness list")
	}
}

func wsMuxWaiter(load int, pressure func() int) *muxQueueWaiter {
	w := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, 128), pressure: pressure}
	for range load + 1 { // active relays plus the opener's reserved slot
		w.slots <- struct{}{}
	}
	return w
}

func takeScheduledWSMux(t *testing.T, q *muxLocalQueue, w *muxQueueWaiter, id int64) {
	t.Helper()
	select {
	case <-w.ready:
	default:
		t.Fatal("an available carrier did not receive its queued user immediately")
	}
	if local, ok := q.popFor(w); !ok || local.timeCreated != id {
		t.Fatalf("FIFO changed: got %d, %v; want %d", local.timeCreated, ok, id)
	}
}

func TestWSMuxQueueSpreadsNewUsersAcrossEveryAvailableCarrier(t *testing.T) {
	q := newBalancedMuxLocalQueue(4)
	carriers := make([]*muxQueueWaiter, 4)
	for i := range carriers {
		carriers[i] = wsMuxWaiter(0, nil)
		carriers[i].lastUse = uint64(i)
		q.wait(carriers[i])
	}
	for i, carrier := range carriers {
		id := int64(i + 1)
		if !q.push(LocalTCPConn{timeCreated: id}, nil) {
			t.Fatal("available queue refused the user")
		}
		takeScheduledWSMux(t, q, carrier, id)
		carrier.slots <- struct{}{} // this user's relay remains active
		q.wait(carrier)
	}
	if q.warmTimer != nil {
		t.Fatal("WebSocket users waited for a warm pair while other carriers were available")
	}
}

func TestWSMuxQueueAvoidsAStalledSocketEvenWhenOtherStreamsAreQuiet(t *testing.T) {
	q := newBalancedMuxLocalQueue(2)
	pressure := 64
	stalled := wsMuxWaiter(0, func() int { return pressure })
	draining := wsMuxWaiter(100, func() int { return 0 })
	q.wait(stalled)
	q.wait(draining)
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	takeScheduledWSMux(t, q, draining, 1)
	q.wait(draining)
	pressure = 0 // the socket recovered; it is eligible again without a restart
	q.push(LocalTCPConn{timeCreated: 2}, nil)
	takeScheduledWSMux(t, q, stalled, 2)
}

func TestWSMuxQueueReassignsUsersWhenTheSelectedCarrierLeaves(t *testing.T) {
	q := newBalancedMuxLocalQueue(2)
	first, replacement := wsMuxWaiter(0, nil), wsMuxWaiter(0, nil)
	first.lastUse, replacement.lastUse = 1, 2
	q.wait(first)
	q.wait(replacement)
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	q.push(LocalTCPConn{timeCreated: 2}, nil)
	if q.selected != first {
		t.Fatal("test did not select the first carrier")
	}
	q.leave(first)
	takeScheduledWSMux(t, q, replacement, 1)
	q.wait(replacement)
	takeScheduledWSMux(t, q, replacement, 2)
}

func TestWSMuxQueueDoesNotGiveAUserToAClosedCarrierReportingZeroPressure(t *testing.T) {
	q := newBalancedMuxLocalQueue(2)
	done := make(chan struct{})
	closed := wsMuxWaiter(0, func() int { return 0 })
	closed.done = done
	live := wsMuxWaiter(100, func() int { return 0 })
	q.wait(closed)
	q.wait(live)
	close(done)
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	takeScheduledWSMux(t, q, live, 1)
	select {
	case <-closed.ready:
		t.Fatal("a closed socket's zero pressure attracted a new user")
	default:
	}
}

func TestWSMuxQueuePreservesFIFOWhenACarrierClosesAfterNotification(t *testing.T) {
	q := newBalancedMuxLocalQueue(2)
	done := make(chan struct{})
	closing, live := wsMuxWaiter(0, nil), wsMuxWaiter(1, nil)
	closing.done = done
	q.wait(closing)
	q.wait(live)
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	<-closing.ready // its worker woke before the failure was published
	close(done)
	q.push(LocalTCPConn{timeCreated: 2}, nil)
	if _, ok := q.popFor(closing); ok {
		t.Fatal("a closing carrier stole a user after its wake-up")
	}
	takeScheduledWSMux(t, q, live, 1)
	q.wait(live)
	takeScheduledWSMux(t, q, live, 2)
}

func TestMuxQueueNeverWaitsForAClosedWarmCarrier(t *testing.T) {
	q, _, spare := muxQueueWithWarmOpeners()
	done := make(chan struct{})
	close(done)
	for _, warm := range q.warm {
		warm.done = done
	}
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	takeScheduledWSMux(t, q, spare, 1)
	if q.warmTimer != nil {
		t.Fatal("a departed warm carrier delayed a live spare")
	}
}

func BenchmarkWSMuxCarrierSelection(b *testing.B) {
	for _, count := range []int{8, 32, 128} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			q := newBalancedMuxLocalQueue(1)
			for range count {
				q.wait(wsMuxWaiter(0, func() int { return 0 }))
			}
			q.entries.PushBack(LocalTCPConn{})
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				q.mu.Lock()
				q.notify()
				selected := q.selected
				<-selected.ready
				q.lastUse++
				selected.lastUse = q.lastUse
				q.selected = nil
				q.mu.Unlock()
			}
		})
	}
}
