package transport

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

type muxWriteObserved struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *muxWriteObserved) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestMuxPairingExpiresWhileSenderIsBlocked(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	wire := &muxWriteObserved{Conn: raw, started: make(chan struct{})}
	cfg := smux.DefaultConfig()
	cfg.KeepAliveDisabled = true
	session, err := smux.Client(wire, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	local, user := net.Pipe()
	defer local.Close()
	defer user.Close()
	limits := newLimiter(Limits{MaxConnections: 1})
	if !limits.acquire() {
		t.Fatal("could not reserve the test connection")
	}
	queue := newMuxLocalQueue(1)
	streams, sessions := int32(1), int32(1)
	loop := muxSession{
		ctx: t.Context(), local: queue, muxCon: 1, limits: limits,
		log: quietLogger(), streams: &streams, sessions: &sessions, reqNewConn: make(chan struct{}, 1),
	}
	f := portForwarder{muxQueue: queue, expireQueued: loop.dropLocal}
	if !f.offer(LocalTCPConn{
		conn: local, remoteAddr: "127.0.0.1:80",
		timeCreated: nowMillis() - (pairingTimeout - 400*time.Millisecond).Milliseconds(),
	}) {
		t.Fatal("could not queue the test connection")
	}
	done := make(chan struct{})
	go func() { defer close(done); loop.run(session) }()
	select {
	case <-wire.started:
	case <-time.After(time.Second):
		t.Fatal("smux never tried to open a stream")
	}
	// Nothing reads the smux socket, so OpenStream is still blocked when the
	// local connection expires. The entire session must stay open.
	user.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := user.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("blocked stream open retained an expired local socket: %v", err)
	}
	if session.IsClosed() {
		t.Fatal("one expired local connection closed the shared mux session")
	}
	session.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mux worker did not stop after closing the session")
	}
	if queue.entries.Len() != 0 || limits.active.Load() != 0 || atomic.LoadInt32(&streams) != 0 || atomic.LoadInt32(&sessions) != 0 {
		t.Fatal("expired pairing was requeued or retained a slot")
	}
}

func TestMuxQueueExpiresWithoutAnySession(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "first attempt", true: "requeued"}[retry], func(t *testing.T) {
			limits := newLimiter(Limits{MaxConnections: 1})
			if !limits.acquire() {
				t.Fatal("could not reserve the test connection")
			}
			local, user := net.Pipe()
			defer local.Close()
			defer user.Close()
			queue := newMuxLocalQueue(1)
			var streams int32
			loop := muxSession{limits: limits, log: quietLogger(), streams: &streams}
			expiredDone := make(chan struct{}, 1)
			f := portForwarder{
				muxQueue: queue,
				expireQueued: func(c LocalTCPConn) {
					loop.dropLocal(c)
					expiredDone <- struct{}{}
				},
				queued: func() { atomic.AddInt32(&streams, 1) },
			}
			if !f.offer(LocalTCPConn{conn: local, timeCreated: nowMillis() - (pairingTimeout - 300*time.Millisecond).Milliseconds()}) {
				t.Fatal("could not queue the test connection")
			}
			if retry {
				request, _ := queue.pop()
				if !request.pairing.claim() || !requeueLocal(queue, request, limits, loop.log) {
					t.Fatal("could not requeue the test connection")
				}
			}
			user.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := user.Read(make([]byte, 1)); err != io.EOF {
				t.Fatalf("unserved request remained open: %v", err)
			}
			if _, ok := queue.pop(); ok {
				t.Fatal("expired entry remained in the queue")
			}
			select {
			case <-expiredDone:
			case <-time.After(time.Second):
				t.Fatal("expiry did not finish")
			}
			if limits.active.Load() != 0 || atomic.LoadInt32(&streams) != 0 {
				t.Fatal("unserved request kept its quota or stream count")
			}
			if !limits.acquire() {
				t.Fatal("the expired slot was not available to a new connection")
			}
			next, nextPeer := net.Pipe()
			defer nextPeer.Close()
			if !f.offer(LocalTCPConn{conn: next, timeCreated: nowMillis()}) {
				next.Close()
				t.Fatal("a new connection could not use the expired queue entry's place")
			}
			request, ok := queue.pop()
			if !ok || request.conn != next || !request.pairing.claim() {
				t.Fatal("the new connection was not available to pair")
			}
			next.Close()
			limits.release()
		})
	}
}

func TestMuxQueueWakesWorkersForEachPendingRequestInOrder(t *testing.T) {
	queue := newMuxLocalQueue(3)
	targets := []string{"first", "second", "third"}
	for _, target := range targets {
		if !queue.push(LocalTCPConn{remoteAddr: target}, nil) {
			t.Fatal("queue filled early")
		}
	}
	for _, target := range targets {
		select {
		case <-queue.ready:
		default:
			t.Fatal("pending request has no worker wakeup")
		}
		request, ok := queue.pop()
		if !ok || request.remoteAddr != target {
			t.Fatalf("next request was %q, want %q", request.remoteAddr, target)
		}
	}
}
