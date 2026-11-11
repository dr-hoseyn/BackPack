package transport

import (
	"testing"
	"time"
)

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

func TestMuxQueueWaitsForAnOpeningWarmCarrier(t *testing.T) {
	q, warm, spare := muxQueueWithWarmOpeners()
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.clearWarmWait()

	q.entries.PushBack(LocalTCPConn{timeCreated: 1})
	q.notify()
	if q.selected != nil || q.warmTimer == nil {
		t.Fatal("did not wait for the warm carrier")
	}
	q.waiters[warm] = struct{}{}
	q.notify()
	if q.selected != warm {
		t.Fatal("the warm carrier did not get the queued connection on return")
	}
	select {
	case <-spare.ready:
		t.Fatal("woke a spare while the warm carrier was returning")
	default:
	}
}

func TestMuxQueueFallsBackOnceWarmWaitExpires(t *testing.T) {
	q, _, spare := muxQueueWithWarmOpeners()
	t.Cleanup(func() { q.leave(spare) })

	if !q.push(LocalTCPConn{timeCreated: 1}, nil) || !q.push(LocalTCPConn{timeCreated: 2}, nil) {
		t.Fatal("failed to queue both connections")
	}
	select {
	case <-spare.ready:
	case <-time.After(time.Second):
		t.Fatal("an unavailable warm carrier stalled the spare")
	}
	if local, ok := q.popFor(spare); !ok || local.timeCreated != 1 {
		t.Fatal("lost the first queued connection")
	}
	q.wait(spare)
	select {
	case <-spare.ready:
		if local, ok := q.popFor(spare); !ok || local.timeCreated != 2 {
			t.Fatal("lost the second queued connection")
		}
	default:
		t.Fatal("a single burst waited for warm carriers twice")
	}
}

func TestMuxQueueReusesSpareUntilItHasOverlappingWork(t *testing.T) {
	q, _, used := muxQueueWithWarmOpeners()
	for _, warm := range q.warm {
		for len(warm.slots) < cap(warm.slots) {
			warm.slots <- struct{}{}
		}
	}
	used.lastUse = 1
	q.lastUse = 1
	fresh := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, 1)}
	fresh.slots <- struct{}{}
	q.wait(fresh)

	q.push(LocalTCPConn{timeCreated: 1}, nil)
	select {
	case <-used.ready:
		q.popFor(used)
	default:
		t.Fatal("did not reuse the spare")
	}
	for range num3SpareReuse + 1 {
		used.slots <- struct{}{}
	}
	q.wait(used)
	q.push(LocalTCPConn{timeCreated: 2}, nil)
	select {
	case <-fresh.ready:
		q.popFor(fresh)
	default:
		t.Fatal("busy spare prevented an available carrier from taking work")
	}
}

func TestMuxQueueAvoidsACarrierWithASendBacklog(t *testing.T) {
	q, congested, spare := muxQueueWithWarmOpeners()
	congested.pressure = func() int { return 64 }
	q.wait(congested)
	other := q.warm[1]
	for len(other.slots) < cap(other.slots) {
		other.slots <- struct{}{}
	}
	if !q.push(LocalTCPConn{timeCreated: 1}, nil) {
		t.Fatal("failed to queue the request")
	}
	select {
	case <-spare.ready:
		q.popFor(spare)
	default:
		t.Fatal("a warm carrier's queued data blocked an available spare")
	}
}

func TestMuxIdleCheckKeepsTheWarmCarrier(t *testing.T) {
	q, warm, _ := muxQueueWithWarmOpeners()
	q.wait(warm)
	q.unwait(warm)
	if !warm.warm || q.warm[0] != warm {
		t.Fatal("an idle check discarded the warm carrier")
	}
	q.leave(warm)
	if warm.warm || q.warm[0] != nil {
		t.Fatal("a departed carrier retained its warm position")
	}
}

func muxQueueWithWarmOpeners() (*muxLocalQueue, *muxQueueWaiter, *muxQueueWaiter) {
	q := newMuxLocalQueue(2)
	q.waiters = make(map[*muxQueueWaiter]struct{})
	warm := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, 4)}
	other := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, 4)}
	spare := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, 4)}
	warm.slots <- struct{}{} // both warm workers are opening streams
	other.slots <- struct{}{}
	spare.slots <- struct{}{} // the spare has reserved a slot and is waiting
	q.warmCarrier(warm)
	q.warmCarrier(other)
	q.waiters[spare] = struct{}{}
	return q, warm, spare
}
