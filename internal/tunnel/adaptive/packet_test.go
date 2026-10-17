//go:build !windows

package adaptive

import (
	"net"
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
