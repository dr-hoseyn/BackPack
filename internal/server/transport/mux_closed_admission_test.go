package transport

import (
	"net"
	"testing"

	"github.com/xtaci/smux"
)

func TestClosedMuxCarrierHandsItsSharedWakeToALiveWorker(t *testing.T) {
	q := newMuxLocalQueue(2)
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	q.push(LocalTCPConn{timeCreated: 2}, nil)
	<-q.wait(nil) // a worker was notified just before its session ended
	done := make(chan struct{})
	close(done)
	if _, ok := q.popForSession(nil, done); ok {
		t.Fatal("closed session consumed a waiting user's connection")
	}
	for _, id := range []int64{1, 2} {
		select {
		case <-q.wait(nil):
		default:
			t.Fatal("closed worker lost the wake-up for a live replacement")
		}
		if local, ok := q.pop(); !ok || local.timeCreated != id {
			t.Fatal("closure changed the user's FIFO position")
		}
	}
}

func TestClosedWarmMuxCarriersDoNotDelayALiveSpare(t *testing.T) {
	q, _, spare := muxQueueWithWarmOpeners()
	done := make(chan struct{})
	close(done)
	for _, warm := range q.warm {
		warm.done = done
	}
	q.push(LocalTCPConn{timeCreated: 1}, nil)
	select {
	case <-spare.ready:
	default:
		t.Fatal("closed warm carriers kept a ready spare waiting")
	}
	if q.warmTimer != nil {
		t.Fatal("a closed carrier started a warm-up grace period")
	}
	if local, ok := q.popFor(spare); !ok || local.timeCreated != 1 {
		t.Fatal("live spare did not receive the user")
	}
}

func TestClosedSelectedMuxCarrierPreservesFIFOForItsReplacement(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_notification", true: "after_notification"}[consumed], func(t *testing.T) {
			q := newMuxLocalQueue(2)
			q.waiters = make(map[*muxQueueWaiter]struct{})
			done := make(chan struct{})
			old := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, 1), done: done}
			live := &muxQueueWaiter{ready: make(chan struct{}, 1), slots: make(chan struct{}, 1)}
			q.wait(old)
			q.push(LocalTCPConn{timeCreated: 1}, nil)
			if consumed {
				<-old.ready
			}
			close(done)
			q.wait(live)
			q.push(LocalTCPConn{timeCreated: 2}, nil)
			if _, ok := q.popFor(old); ok {
				t.Fatal("closed selected carrier consumed a user")
			}
			for _, id := range []int64{1, 2} {
				select {
				case <-live.ready:
				default:
					t.Fatal("live replacement did not receive notification")
				}
				if local, ok := q.popFor(live); !ok || local.timeCreated != id {
					t.Fatal("replacement did not receive users in their original order")
				}
				q.wait(live)
			}
		})
	}
}

func TestAlreadyClosedMuxSessionsLeaveQueuedUsersAndPoolDemandUntouched(t *testing.T) {
	for range 32 {
		conn, peer := net.Pipe()
		cfg := smux.DefaultConfig()
		cfg.KeepAliveDisabled = true
		session, err := smux.Client(conn, cfg)
		if err != nil {
			peer.Close()
			t.Fatal(err)
		}
		session.Close()
		peer.Close()
		q := newMuxLocalQueue(2)
		q.push(LocalTCPConn{timeCreated: 1}, nil)
		q.push(LocalTCPConn{timeCreated: 2}, nil)
		streams, sessions := int32(2), int32(1)
		requests := make(chan struct{}, 1)
		loop := muxSession{ctx: t.Context(), local: q, muxCon: 1,
			log: quietLogger(), streams: &streams, sessions: &sessions, reqNewConn: requests}
		loop.run(session)
		if sessions != 0 || streams != 2 || len(requests) != 0 {
			t.Fatal("closed session consumed quota or invented new pool demand")
		}
		for _, id := range []int64{1, 2} {
			if local, ok := q.pop(); !ok || local.timeCreated != id {
				t.Fatal("closed session reordered or removed a queued user")
			}
		}
	}
}
