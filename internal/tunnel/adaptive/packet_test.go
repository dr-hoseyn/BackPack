//go:build !windows

package adaptive

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/pathprobe"
	"github.com/backpack/backpack/internal/tunnel/naive"
)

func TestUnavailableNativeAssociationReportsADropInsteadOfSuccessfulBytes(t *testing.T) {
	selector, err := NewSelector(config.PathPolicy{}, []bool{true})
	if err != nil {
		t.Fatal(err)
	}
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3080}
	p := &packetConn{parent: &ClientPaths{selector: selector}, ctx: t.Context(), peer: peer, pcs: map[int]*naive.SOCKSPacketConn{}}
	check := func() {
		t.Helper()
		if n, err := p.WriteTo([]byte("unavailable"), peer); n != 0 || err == nil {
			t.Fatalf("unavailable path reported %d sent bytes: %v", n, err)
		}
	}
	check() // no verified candidate
	selector.Record(0, pathprobe.Result{RTT: time.Millisecond, BytesPerSecond: 1e6}, nil, time.Now())
	check() // verified candidate, local association not ready yet
}

func TestEconomyNativeAssociationsReleaseParentReferencesOnClose(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := &ClientPaths{ctx: ctx, cfg: config.ClientConfig{RemoteAddr: "127.0.0.1:3080", Paths: []config.ManagedClientPath{{Sing: config.SingClientConfig{Protocol: "hysteria2", NativeUDP: true}}}}, paths: make([]clientPath, 1)}
	var connections []net.PacketConn
	for range 64 {
		pc, _, err := c.PacketConn()
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, pc)
	}
	var wg sync.WaitGroup
	for _, pc := range connections {
		wg.Go(func() { pc.Close() })
	}
	wg.Wait()
	c.mu.Lock()
	if len(c.packets) != 0 {
		t.Errorf("retained %d closed associations", len(c.packets))
	}
	for _, p := range c.packets[:cap(c.packets)] {
		if p != nil {
			t.Error("backing array retained a closed connection")
		}
	}
	c.mu.Unlock()
	cancel()
	if pc, _, err := c.PacketConn(); pc != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatal("closed parent created another association")
	}
}
