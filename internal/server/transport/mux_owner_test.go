package transport

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

func TestDequeuedMuxSessionKeepsItsOwnerAcrossClientReplacement(t *testing.T) {
	for _, mode := range []string{"tcpmux", "wsmux", "kcp"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var seat clientSeat
			run := func(ctx context.Context, _ func()) { <-ctx.Done() }
			seat.sit(ctx, func() {}, func() {}, run)
			owner := seat.client()
			conn, peer := net.Pipe()
			defer peer.Close()
			session, err := smux.Client(conn, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			queue := make(chan pooledMuxSession, 1)
			if !seat.admitFor(ctx, owner, func(ctx context.Context) bool {
				return enqueueMuxSession(queue, ctx, session)
			}) {
				t.Fatal("live client's session was rejected")
			}
			pending := <-queue
			// The worker has dequeued the session, so draining the old pool
			// cannot find it, but it has not built the session loop yet.
			seat.sit(ctx, func() { drainTunnelConns(queue) }, func() {}, run)
			local := make(chan LocalTCPConn, 1)
			user, userPeer := net.Pipe()
			defer user.Close()
			defer userPeer.Close()
			local <- LocalTCPConn{conn: user, timeCreated: nowMillis(), remoteAddr: "127.0.0.1:80"}
			log := logrus.New()
			log.SetLevel(logrus.FatalLevel)
			var loop muxSession
			var sessions *int32
			switch mode {
			case "tcpmux":
				s := &TcpMuxTransport{lifecycle: lifecycle{logger: log}, config: &TcpMuxConfig{MuxCon: 1}}
				loop = s.session(&tcpMuxGen{ctx: ctx, localChannel: local}, pending.owner)
				sessions = &s.sessionCounter
			case "wsmux":
				s := &WsMuxTransport{lifecycle: lifecycle{logger: log}, config: &WsMuxConfig{MuxCon: 1}}
				loop = s.session(&wsMuxGen{ctx: ctx, localChannel: local}, pending.owner)
				sessions = &s.sessionCounter
			case "kcp":
				s := &KcpTransport{lifecycle: lifecycle{logger: log}, config: &KcpConfig{MuxCon: 1}}
				loop = s.session(&kcpGen{ctx: ctx, localChannel: local}, pending.owner)
				sessions = &s.sessionCounter
			}
			if loop.ctx != owner || loop.ctx.Err() == nil || seat.client().Err() != nil {
				t.Fatal("dequeued session was rebound to the replacement client")
			}
			atomic.StoreInt32(sessions, 1)
			done := make(chan struct{})
			go func() { loop.run(pending.Session); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("old session worker survived client replacement")
			}
			if len(local) != 1 || atomic.LoadInt32(sessions) != 0 {
				t.Fatal("retired session consumed a new user's connection or leaked its counter")
			}
			select {
			case <-session.CloseChan():
			default:
				t.Fatal("retired session's socket remained open")
			}
		})
	}
}

func TestQueuedMuxSessionClosesWhenItsOwnerEnds(t *testing.T) {
	owner, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, peer := net.Pipe()
	defer peer.Close()
	session, err := smux.Client(conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	queue := make(chan pooledMuxSession, 1)
	if !enqueueMuxSession(queue, owner, session) {
		t.Fatal("live owner rejected")
	}
	cancel()
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("queued session retained its socket after owner cancellation")
	}
}
