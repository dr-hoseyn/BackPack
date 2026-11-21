package transport

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils"
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
	testMuxPairingExpiresWhileSenderIsBlocked(t, newMuxLocalQueue(1))
}

func TestWSMuxSlowOpenWithoutASiblingKeepsTheOriginalDeadline(t *testing.T) {
	queue := newBalancedMuxLocalQueue(1)
	queue.retrySlowOpen = true
	testMuxPairingExpiresWhileSenderIsBlocked(t, queue)
}

func testMuxPairingExpiresWhileSenderIsBlocked(t *testing.T, queue *muxLocalQueue) {
	t.Helper()
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
	streams, sessions := int32(1), int32(1)
	loop := muxSession{
		ctx: t.Context(), local: queue, muxCon: 1, limits: limits,
		log: quietLogger(), streams: &streams, sessions: &sessions, reqNewConn: make(chan struct{}, 1),
	}
	f := portForwarder{muxQueue: queue, expireQueued: loop.dropLocal}
	if !f.offer(LocalTCPConn{
		conn: local, remoteAddr: "127.0.0.1:80",
		timeCreated: nowMillis() - (pairingTimeout - 800*time.Millisecond).Milliseconds(),
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
	if queue.retrySlowOpen {
		// No sibling is waiting. The retry check must not close the user
		// before the original pairing deadline.
		_ = user.SetReadDeadline(time.Now().Add(slowMuxOpenDelay + 150*time.Millisecond))
		var timeout net.Error
		if _, err := user.Read(make([]byte, 1)); !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("retry check dropped a user with no sibling: %v", err)
		}
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

func TestWSMuxRetriesAStalledOpenOnAnAvailableSibling(t *testing.T) {
	cfg := smux.DefaultConfig()
	cfg.KeepAliveDisabled = true
	cfg.Version = 2
	stalledWire, unread := net.Pipe()
	t.Cleanup(func() { unread.Close() })
	stalled := &muxWriteObserved{Conn: stalledWire, started: make(chan struct{})}
	first, err := smux.Client(stalled, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	left, right := net.Pipe()
	healthy, err := smux.Client(left, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	peer, err := smux.Server(right, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	peer.SetDeadline(time.Now().Add(time.Second))

	local, user := net.Pipe()
	defer user.Close()
	defer local.Close()
	limits := newLimiter(Limits{MaxConnections: 1})
	if !limits.acquire() {
		t.Fatal("could not reserve user quota")
	}
	queue := newBalancedMuxLocalQueue(4)
	queue.retrySlowOpen = true
	streams, sessions := int32(1), int32(2)
	loop := muxSession{
		ctx: t.Context(), local: queue, muxCon: 2, limits: limits,
		log: quietLogger(), streams: &streams, sessions: &sessions,
	}
	forward := portForwarder{muxQueue: queue, expireQueued: loop.dropLocal}
	firstDone := make(chan struct{})
	go func() { defer close(firstDone); loop.run(first) }()
	const target = "127.0.0.1:8081"
	if !forward.offer(LocalTCPConn{conn: local, remoteAddr: target, timeCreated: nowMillis()}) {
		t.Fatal("could not queue user")
	}
	select {
	case <-stalled.started:
	case <-time.After(time.Second):
		t.Fatal("the first carrier did not start opening a stream")
	}

	healthyDone := make(chan struct{})
	go func() { defer close(healthyDone); loop.run(healthy) }()
	stream, err := peer.AcceptStream()
	if err != nil {
		t.Fatalf("healthy carrier did not receive the request: %v", err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(time.Second))
	if got, err := utils.ReceiveBinaryString(stream); err != nil || got != target {
		t.Fatalf("healthy carrier received destination %q, err %v", got, err)
	}
	if first.IsClosed() || limits.active.Load() != 1 {
		t.Fatal("reassigning the user closed the old carrier or released the active quota")
	}

	request := []byte("a verified request")
	_ = user.SetDeadline(time.Now().Add(time.Second))
	write := make(chan error, 1)
	go func() { _, err := user.Write(request); write <- err }()
	buf := make([]byte, len(request))
	if _, err := io.ReadFull(stream, buf); err != nil || string(buf) != string(request) {
		t.Fatalf("relayed request %q, err %v", buf, err)
	}
	if err := <-write; err != nil {
		t.Fatal(err)
	}

	// Let the blocked OpenStream finally succeed. Its peer may receive
	// the SYN, but it must get EOF before any destination was announced.
	oldPeer, err := smux.Server(unread, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer oldPeer.Close()
	oldPeer.SetDeadline(time.Now().Add(time.Second))
	orphan, err := oldPeer.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	_ = orphan.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := orphan.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("abandoned stream sent data, got %d bytes and %v", n, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	_ = user.Close()
	_ = healthy.Close()
	for _, done := range []<-chan struct{}{firstDone, healthyDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("mux admission worker did not stop")
		}
	}
	if limits.active.Load() != 0 || atomic.LoadInt32(&streams) != 0 || atomic.LoadInt32(&sessions) != 0 {
		t.Fatal("the retry retained a slot or connection count")
	}
}
