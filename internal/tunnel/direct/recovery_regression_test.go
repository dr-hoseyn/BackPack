package direct

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
)

func TestRecoveryRegressionForwarderBindFailure(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	edge, err := NewEdge(Config{Addr: "127.0.0.1:1", Token: "token", Ports: []string{held.Addr().String() + "=127.0.0.1:1234"}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- edge.Run(ctx) }()
	select {
	case err := <-done:
		cancel()
		if err == nil {
			t.Fatal("a failed listener returned no error")
		}
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("a failed listener never reached the engine's restart path")
	}
}

func TestRecoveryRegressionOriginHandshakeCancellation(t *testing.T) {
	origin, err := NewOrigin(Config{Addr: "127.0.0.1:0", Token: "token"}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- origin.Run(ctx) }()
	defer cancel()
	conn, err := net.Dial("tcp", awaitBind(t, origin).String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		conn.Close()
		<-done
		t.Fatal("origin shutdown waited for the handshake timeout")
	}
}

func TestRecoveryRegressionEdgeHandshakeCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	edge, err := NewEdge(Config{Addr: listener.Addr().String(), Token: "token", Ports: []string{fmt.Sprintf("127.0.0.1:%d=1234", freePort(t))}}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- edge.Run(ctx) }()
	defer cancel()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("edge never dialled")
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		conn.Close()
		<-done
		t.Fatal("edge shutdown waited for the handshake timeout")
	}
}

func TestRecoveryRegressionReportsAuthenticatedSessions(t *testing.T) {
	metrics.ClearPeer()
	tn := newTunnel(t, TransportTCP, "shared-token", "shared-token", echoBackend(t, "S:"))
	tn.awaitSession(t, 5*time.Second)
	if got := metrics.SnapshotConnected(); got == nil || !*got {
		t.Fatal("the direct engine did not publish its authenticated session")
	}
	if metrics.SnapshotPeer() == "" {
		t.Fatal("the direct engine did not publish its peer")
	}
}

func TestRecoveryRegressionReconnectOnTheSamePort(t *testing.T) {
	for _, transport := range allTransports {
		t.Run(transport, func(t *testing.T) {
			backend := echoBackend(t, "S:")
			udpBackend := echoUDPBackend(t, "U:")
			originAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
			startOrigin := func() (context.CancelFunc, <-chan error) {
				origin, err := NewOrigin(Config{Addr: originAddr, Token: "token", Transport: transport}, testLogger(t))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- origin.Run(ctx) }()
				awaitBind(t, origin)
				return cancel, done
			}
			stopOrigin, originDone := startOrigin()
			defer func() { stopOrigin(); <-originDone }()
			port, udpPort := freePort(t), freePort(t)
			edge, err := NewEdge(Config{Addr: originAddr, Token: "token", Transport: transport, MaxConnections: 2, AcceptUDP: true,
				RetryDelay: 50 * time.Millisecond, Ports: []string{fmt.Sprintf("127.0.0.1:%d=%s", port, backend), fmt.Sprintf("127.0.0.1:%d=%s", udpPort, udpBackend)}}, testLogger(t))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- edge.Run(ctx) }()
			defer func() { cancel(); <-done }()
			tn := &tunnel{edge: edge, port: port}
			tn.awaitSession(t, 5*time.Second)
			if got := tn.roundTrip(t, "before"); got != "S:before" {
				t.Fatal(got)
			}
			udp := (&tunnel{port: udpPort}).dialUDP(t)
			if got, ok := udpExchange(t, udp, "before", 10); !ok || got != "U:before" {
				t.Fatalf("udp before: %q %v", got, ok)
			}
			idle, err := net.Dial("tcp", tn.addr())
			if err != nil {
				t.Fatal(err)
			}
			defer idle.Close()
			for deadline := time.Now().Add(3 * time.Second); edge.Stats().Active != 2 && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			if edge.Stats().Active != 2 {
				t.Fatalf("idle connections not admitted: %+v", edge.Stats())
			}
			stopOrigin()
			<-originDone
			for deadline := time.Now().Add(5 * time.Second); (edge.Stats().Sessions != 0 || edge.limiter.Active() != 0) && time.Now().Before(deadline); {
				time.Sleep(10 * time.Millisecond)
			}
			if edge.Stats().Sessions != 0 || edge.limiter.Active() != 0 {
				t.Fatalf("dead session retained capacity: %+v slots=%d", edge.Stats(), edge.limiter.Active())
			}
			stopOrigin, originDone = startOrigin()
			tn.awaitSession(t, 5*time.Second)
			if got := tn.roundTrip(t, "after"); got != "S:after" {
				t.Fatal(got)
			}
			if got, ok := udpExchange(t, udp, "after", 10); !ok || got != "U:after" {
				t.Fatalf("udp after: %q %v", got, ok)
			}
		})
	}
}
