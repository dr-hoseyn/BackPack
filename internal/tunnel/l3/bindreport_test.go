package l3

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// A listener that cannot bind has to say so.
//
// This is what a CI failure in this package actually was, twice diagnosed as a
// flaky UDP test. The port picked for the test was taken between being checked
// and being bound, the UDP listener never came up, and the forwarder said
// nothing at all — Run captured the error and then threw it away, because it
// returned nil whenever the context had since been cancelled, which is every
// shutdown. TCP bound fine, so the forwarder went on working in a way that
// looked entirely healthy while half of what it was asked to carry did not
// exist.
//
// From the outside that is a port that quietly does nothing, which no amount of
// retrying at the other end can fix.
func TestAUDPListenerThatCannotBindIsReported(t *testing.T) {
	held, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not hold the port: %v", err)
	}
	defer held.Close()
	addr := held.LocalAddr().String()

	backend := echoUDP(t, "U:")
	log, captured := testLoggerCapturing(t)
	forwarder, err := NewForwarder(Config{
		Ports:     []string{addr + "=" + backend},
		AcceptUDP: true, PeerIP: "127.0.0.1",
	}, log)
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- forwarder.Run(ctx) }()

	select {
	case err := <-runErr:
		var bindErr *net.OpError
		if !errors.As(err, &bindErr) || bindErr.Op != "listen" || bindErr.Net != "udp" ||
			bindErr.Addr == nil || bindErr.Addr.String() != addr {
			t.Errorf("Run returned %v, want UDP listener error for %s", err, addr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}

	if out := captured.String(); !strings.Contains(out, addr) {
		t.Errorf("nothing in the log names the port that could not be bound:\n%s", out)
	}
}

// The healthy case must stay quiet: a shutdown is not a failure.
func TestAnOrdinaryShutdownIsNotAnError(t *testing.T) {
	port := freePort(t)
	backend := echoUDP(t, "U:")
	log, _ := testLoggerCapturing(t)
	forwarder, err := NewForwarder(Config{
		Ports:     []string{fmt.Sprintf("127.0.0.1:%d=%s", port, backend)},
		AcceptUDP: true, PeerIP: "127.0.0.1",
	}, log)
	if err != nil {
		t.Fatalf("NewForwarder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- forwarder.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("a clean shutdown reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}
}
