package direct

import (
	"context"
	"errors"
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
