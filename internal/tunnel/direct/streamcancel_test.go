package direct

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/tunnel/portmap"
	"github.com/xtaci/smux"
)

func TestCanceledStreamSetupKeepsTheSharedSession(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	client, err := smux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := smux.Server(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	request := make(chan error, 1)
	go func() {
		stream, err := peer.AcceptStream()
		if err == nil {
			_, _, err = readRequest(stream)
		}
		// Keep the stream open without acknowledging backend setup.
		request <- err
	}()
	edge := &Edge{cfg: Config{DialTimeout: 10 * time.Second}}
	edge.addSession(&tunnelSession{Session: client})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := edge.openFor(ctx, kindUDP, portmap.Mapping{Targets: []string{"127.0.0.1:80", "127.0.0.1:81"}})
		result <- err
	}()
	select {
	case err := <-request:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the stream request never reached the peer")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled stream setup returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled stream setup kept waiting for the peer")
	}
	if client.IsClosed() || peer.IsClosed() {
		t.Fatal("canceling one stream closed its shared session")
	}
}

func TestDirectRetriesTheSameTargetAfterSessionLoss(t *testing.T) {
	makePair := func() (*tunnelSession, *smux.Session) {
		t.Helper()
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		watched := newWatchedConn(a, time.Second)
		client, err := smux.Client(watched, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { client.Close() })
		peer, err := smux.Server(b, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { peer.Close() })
		return &tunnelSession{Session: client, dead: watched.dead}, peer
	}
	first, broken := makePair()
	second, healthy := makePair()
	edge := &Edge{cfg: Config{DialTimeout: time.Second}}
	edge.addSession(first)
	edge.addSession(second)

	const wanted = "127.0.0.1:80"
	failedTarget := make(chan string, 1)
	go func() {
		defer broken.Close()
		stream, err := broken.AcceptStream()
		if err == nil {
			_, target, _ := readRequest(stream)
			failedTarget <- target
		}
	}()
	retriedTarget := make(chan string, 1)
	go func() {
		stream, err := healthy.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		_, target, err := readRequest(stream)
		if err != nil {
			return
		}
		retriedTarget <- target
		_, _ = stream.Write([]byte{streamOK, 'x'})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := edge.openFor(ctx, kindTCP, portmap.Mapping{Targets: []string{wanted, "127.0.0.1:81"}})
	if err != nil {
		t.Fatalf("request failed despite a healthy session: %v", err)
	}
	defer stream.Close()
	_ = stream.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := io.ReadFull(stream, data[:]); err != nil || data[0] != 'x' {
		t.Fatalf("replacement stream returned %q, %v", data, err)
	}
	if firstTarget, nextTarget := <-failedTarget, <-retriedTarget; firstTarget != wanted || nextTarget != wanted {
		t.Fatalf("session retry changed the target from %q to %q", firstTarget, nextTarget)
	}
}
