package transport

import "testing"

func TestMuxQueueReusesWarmCarriersAndSpreadsOverlappingStreams(t *testing.T) {
	q := newMuxLocalQueue(2)
	q.waiters = make(map[*muxQueueWaiter]struct{})
	newWaiter := func() *muxQueueWaiter {
		w := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, num3SpareLoad+2)}
		w.slots <- struct{}{} // reserved by each available worker
		return w
	}
	first, second, spare := newWaiter(), newWaiter(), newWaiter()

	push := func(id int64) {
		t.Helper()
		if !q.push(LocalTCPConn{timeCreated: id}, nil) {
			t.Fatal("failed to queue connection")
		}
	}
	take := func(w *muxQueueWaiter, id int64) {
		t.Helper()
		select {
		case <-w.ready:
		default:
			t.Fatalf("carrier was not selected for connection %d", id)
		}
		local, ok := q.popFor(w)
		if !ok || local.timeCreated != id {
			t.Fatalf("got connection %d, %t; want %d", local.timeCreated, ok, id)
		}
	}

	q.wait(first)
	push(1)
	take(first, 1)

	q.wait(second)
	q.wait(spare)
	q.wait(first)
	push(2)
	take(second, 2)

	q.wait(second)
	push(3)
	take(first, 3)

	for range num3SpareLoad + 1 {
		first.slots <- struct{}{}
		second.slots <- struct{}{}
	}
	q.wait(first)
	push(4)
	push(5)
	take(spare, 4)
	q.leave(second) // cancelled after notification and before taking its work
	take(first, 5)

	q.wait(spare)
	if !spare.warm {
		t.Fatal("a spare did not replace the removed warm carrier")
	}
}
