package l3

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/tunnel/limits"
)

// A cancelled generation must interrupt socket I/O itself. Waiting for the
// listener's deferred cleanup is circular if that listener is stuck writing
// to a backend whose send buffer is full.
func TestUDPFlowCancellationClosesBackendAndReleasesSlots(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  limits.Config
	}{
		{"unlimited", limits.Config{}},
		{"connection-cap", limits.Config{MaxConnections: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			local, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &Forwarder{limiter: limits.New(tc.cfg)}
			pool := newBackendPool([]string{backend.LocalAddr().String()})
			var flows sync.Map
			var workers sync.WaitGroup
			flow, err := f.udpFlowFor(ctx, &flows, local, netip.MustParseAddrPort("127.0.0.1:12345"), pool, &workers)
			if err != nil {
				t.Fatal(err)
			}
			defer flow.close()
			done := make(chan struct{})
			go func() { workers.Wait(); close(done) }()
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				flow.close()
				<-done
				t.Fatal("cancelled flow retained its blocked socket and reply worker")
			}
			if f.Stats().Active != 0 || pool.members[0].active.Load() != 0 || f.limiter.Active() != 0 {
				t.Fatal("cancelled flow retained a connection or backend slot")
			}
			flows.Range(func(_, _ any) bool { t.Error("cancelled flow remains in the flow table"); return true })
			if _, err := flow.backend.Write([]byte("closed")); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("backend socket survived cancellation: %v", err)
			}
		})
	}
}
