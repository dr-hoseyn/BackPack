package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// Large receive windows previously forced this pool back to one physical
// session. Its two admitted streams then blocked every new user, despite free
// CPU, RAM and a healthy control channel. Keep old flows open while new ones
// force demand-driven session growth through the real control channel.
func TestMuxCapacityGrowsWhileExistingForwardsStayOpen(t *testing.T) {
	for _, tr := range []string{"tcpmux", "wsmux", "wssmux", "kcp"} {
		t.Run(tr, func(t *testing.T) {
			backend := startEchoBackend(t)
			tunnelPort, entryPort := freePort(t), freePort(t)
			const token = "capacity-regression-token-0123456789"
			s := baseServerConfig(tr, tunnelPort, entryPort, backend.addr, token)
			c := baseClientConfig(tr, fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil)
			s.MuxCon, c.ConnectionPool = 2, 1
			s.MaxReceiveBuffer, c.MaxReceiveBuffer = 128<<20, 128<<20
			tun := runPair(t, s, c, entryPort, tunnelPort)
			if err := tun.waitReady(tunnelReadyTimeout); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 12; i++ {
				conn, err := net.DialTimeout("tcp", tun.Entry, 2*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.Close() })
				_ = conn.SetDeadline(time.Now().Add(2500 * time.Millisecond))
				payload := []byte("capacity")
				if _, err := conn.Write(payload); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(payload))
				if _, err := io.ReadFull(conn, got); err != nil {
					t.Fatalf("new forward %d stalled behind existing flows: %v", i, err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatal("corrupted forward")
				}
			}
			if err := tun.roundTrip(randomPayload(t, 256<<10)); err != nil {
				t.Fatalf("fresh transfer while old forwards remain open: %v", err)
			}
		})
	}
}
