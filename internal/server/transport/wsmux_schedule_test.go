package transport

import (
	"fmt"
	"testing"
)

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
