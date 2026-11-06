package network

import (
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestWatchedMuxRetiresAnIdleSessionOnPeerClose(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	session, err := NewWatchedMuxSession(local, nil, smux.Client)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	peer.Close()
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("idle session remained in the pool after the socket closed")
	}
}

func TestWatchedMuxInterruptsAStalledSocketWrite(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	cfg := smux.DefaultConfig()
	cfg.KeepAliveDisabled = true
	cfg.KeepAliveTimeout = 30 * time.Millisecond
	session, err := NewWatchedMuxSession(local, cfg, smux.Client)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	done := make(chan error, 1)
	go func() {
		_, err := session.OpenStream()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked stream unexpectedly opened")
		}
	case <-time.After(time.Second):
		t.Fatal("the socket write ignored the mux write limit")
	}
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("the failed socket left its mux session live")
	}
}
