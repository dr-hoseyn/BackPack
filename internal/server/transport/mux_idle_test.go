package transport

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils"
	"github.com/xtaci/smux"
)

func TestMuxIdleRetirementNeverClosesAnOpenStream(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a, b := net.Pipe()
	session, err := smux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	peer, err := smux.Server(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var lifecycle lifecycle
	var sessions int32 = 2
	var streams int32 = 1
	local := make(chan LocalTCPConn, 1)
	user, app := net.Pipe()
	defer app.Close()
	defer user.Close()
	local <- LocalTCPConn{conn: user, timeCreated: nowMillis(), remoteAddr: "127.0.0.1:80"}
	loop := muxSession{
		ctx: ctx, local: local, muxCon: 1, log: quietLogger(),
		sessions: &sessions, streams: &streams, idleTimeout: 30 * time.Millisecond,
		idleRetire: func() bool { return lifecycle.retireIdleSession(ctx, ctx, &sessions, 1) },
	}
	done := make(chan struct{})
	go func() { loop.run(session); close(done) }()
	t.Cleanup(func() { cancel(); session.Close(); <-done })
	stream, err := peer.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if target, err := utils.ReceiveBinaryString(stream); err != nil || target != "127.0.0.1:80" {
		t.Fatalf("target = %q, %v", target, err)
	}
	// Silence for several retirement periods is still an active user.
	select {
	case <-done:
		t.Fatal("idle timer disconnected a live stream")
	case <-time.After(150 * time.Millisecond):
	}
	go func() { _, _ = stream.Write([]byte("still alive")) }()
	_ = app.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, len("still alive"))
	if _, err := io.ReadFull(app, got); err != nil || string(got) != "still alive" {
		t.Fatalf("quiet user's transfer failed: %q, %v", got, err)
	}
	stream.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("excess session was not retired after its final stream ended")
	}
	if atomic.LoadInt32(&sessions) != 1 || atomic.LoadInt32(&streams) != 0 {
		t.Fatalf("retirement counters = %d sessions, %d streams", sessions, streams)
	}
}

func TestConcurrentMuxRetirementPreservesFloorAndGeneration(t *testing.T) {
	ctx := t.Context()
	var lifecycle lifecycle
	var sessions int32 = 32
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() { lifecycle.retireIdleSession(ctx, ctx, &sessions, 8) })
	}
	workers.Wait()
	if sessions != 8 {
		t.Fatalf("retirement crossed floor: %d", sessions)
	}
	old, cancel := context.WithCancel(ctx)
	cancel()
	for _, pair := range [][2]context.Context{{old, ctx}, {ctx, old}} {
		if lifecycle.retireIdleSession(pair[0], pair[1], &sessions, 1) {
			t.Fatal("retired generation changed current counters")
		}
	}
}
