package direct

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The relay keeps both TCP sockets established while discarding application
// bytes. Neither engine is restarted and the forwarded port stays the same.
func TestRecoveryNewSilentPathReconnectsWithoutRecreation(t *testing.T) {
	origin, err := NewOrigin(Config{Addr: "127.0.0.1:0", Token: "token"}, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	originDone := make(chan error, 1)
	go func() { originDone <- origin.Run(ctx) }()
	originAddr := awaitBind(t, origin).String()
	relay, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var dropping atomic.Bool
	var connections atomic.Int64
	var relayWG sync.WaitGroup
	relayWG.Add(1)
	go func() {
		defer relayWG.Done()
		for {
			a, err := relay.Accept()
			if err != nil {
				return
			}
			b, err := net.Dial("tcp", originAddr)
			if err != nil {
				a.Close()
				continue
			}
			connections.Add(1)
			relayWG.Add(1)
			go func() {
				defer relayWG.Done()
				stop := context.AfterFunc(ctx, func() { a.Close(); b.Close() })
				defer stop()
				defer a.Close()
				defer b.Close()
				pump := func(dst, src net.Conn, done chan<- struct{}) {
					defer func() { a.Close(); b.Close(); done <- struct{}{} }()
					buf := make([]byte, 32768)
					for {
						n, err := src.Read(buf)
						if n > 0 && !dropping.Load() {
							if _, err := dst.Write(buf[:n]); err != nil {
								return
							}
						}
						if err != nil {
							return
						}
					}
				}
				done := make(chan struct{}, 2)
				go pump(a, b, done)
				go pump(b, a, done)
				<-done
				<-done
			}()
		}
	}()
	port := freePort(t)
	edge, err := NewEdge(Config{Addr: relay.Addr().String(), Token: "token", RetryDelay: 50 * time.Millisecond,
		Ports: []string{fmt.Sprintf("127.0.0.1:%d=%s", port, echoBackend(t, "S:"))}}, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	edgeDone := make(chan error, 1)
	go func() { edgeDone <- edge.Run(ctx) }()
	t.Cleanup(func() { cancel(); relay.Close(); <-edgeDone; <-originDone; relayWG.Wait() })
	tn := &tunnel{edge: edge, port: port}
	tn.awaitSession(t, 5*time.Second)
	if got := tn.roundTrip(t, "before"); got != "S:before" {
		t.Fatal(got)
	}
	dropping.Store(true)
	for deadline := time.Now().Add(95 * time.Second); edge.Stats().Sessions != 0 && time.Now().Before(deadline); {
		time.Sleep(25 * time.Millisecond)
	}
	if edge.Stats().Sessions != 0 {
		t.Fatal("a silently dead path stayed connected")
	}
	local, err := net.DialTimeout("tcp", tn.addr(), time.Second)
	if err != nil {
		t.Fatalf("the forwarded listener disappeared during recovery: %v", err)
	}
	local.SetDeadline(time.Now().Add(time.Second))
	_, _ = io.Copy(io.Discard, local)
	local.Close()
	dropping.Store(false)
	tn.awaitSession(t, 20*time.Second)
	if connections.Load() < 2 {
		t.Fatal("the edge did not replace its dead connection")
	}
	if got := tn.roundTrip(t, "after"); got != "S:after" {
		t.Fatal(got)
	}
}
