//go:build linux

package network

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestTCPMuxPressureAccountsForQueuedDataAndRTTInflation(t *testing.T) {
	for _, tc := range []struct {
		name string
		info unix.TCPInfo
		want time.Duration
	}{
		{"idle", unix.TCPInfo{}, 0},
		{"queued data", unix.TCPInfo{Notsent_bytes: 100_000, Delivery_rate: 1_000_000}, 100 * time.Millisecond},
		{"slower acknowledgements", unix.TCPInfo{Rtt: 80_000, Min_rtt: 30_000}, 50 * time.Millisecond},
		{"window estimate", unix.TCPInfo{Rtt: 100_000, Min_rtt: 100_000, Snd_cwnd: 10, Snd_mss: 1000, Notsent_bytes: 20_000}, 200 * time.Millisecond},
		{"bounded slow socket", unix.TCPInfo{Notsent_bytes: ^uint32(0), Delivery_rate: 1}, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tcpMuxQueueDelay(&tc.info); got != tc.want {
				t.Fatalf("queue delay = %s, want %s", got, tc.want)
			}
		})
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if pressure := TCPMuxPressure(a)(); pressure != 0 {
		t.Fatalf("a connection without TCP information had pressure %d", pressure)
	}
}
