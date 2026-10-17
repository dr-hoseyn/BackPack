//go:build linux

package network

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This process is invoked twice by mptcp-live.sh in two isolated namespaces.
// One connection must negotiate MPTCP, gain a second subflow, and keep carrying
// verified bytes after the script removes the primary interface.
func TestMPTCPLiveNamespacePeer(t *testing.T) {
	role := os.Getenv("BP_MPTCP_ROLE")
	if role == "" {
		t.Skip("run internal/e2e/testdata/mptcp-live.sh for the isolated two-path test")
	}
	state := os.Getenv("BP_MPTCP_STATE")
	if role == "server" {
		l, err := ListenWithBuffers("tcp", "0.0.0.0:45000", 0, 0, 0, time.Second, false, true)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if err = os.WriteFile(filepath.Join(state, "ready"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(40 * time.Second))
		if ok, err := c.(*net.TCPConn).MultipathTCP(); err != nil || !ok {
			t.Fatalf("MPTCP negotiation required: %v %v", ok, err)
		}
		if _, err = io.Copy(c, c); err != nil {
			t.Fatal(err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	c, err := TcpDialerVia(ctx, &Outbound{MPTCP: true}, "10.221.0.2:45000", 5*time.Second, time.Second, true, 1, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if ok, err := c.MultipathTCP(); err != nil || !ok {
		t.Fatalf("MPTCP negotiation required: %v %v", ok, err)
	}
	if err = os.WriteFile(filepath.Join(state, "negotiated"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	after := 0
	for n := 0; n < 600; n++ {
		payload := bytes.Repeat([]byte(fmt.Sprintf("verified frame %06d;", n)), 128)
		c.SetDeadline(time.Now().Add(12 * time.Second))
		if _, err = c.Write(payload); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		if _, err = io.ReadFull(c, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, got) {
			t.Fatal("same-connection payload changed")
		}
		if _, err = os.Stat(filepath.Join(state, "primary-down")); err == nil {
			after++
		}
		if after >= 40 {
			t.Log("same MPTCP connection verified for two seconds after primary-path removal")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("primary-path outage was not survived")
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("namespace harness did not remove the primary path")
}
