//go:build linux

package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"golang.org/x/sys/unix"
)

func TestWSSMuxForwardedTCPKeepsConfiguredSocketOptions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		idle     time.Duration
		wantIdle int
		nodelay  bool
	}{
		{"configured", 43 * time.Second, 43, false},
		{"default", 0, 15, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			s := listenerRecoveryServer(t, config.WSSMUX, freeAddr(t), ctx)
			t.Cleanup(func() {
				cancel()
				s.listeners.wait(context.Background())
			})
			addr := freeAddr(t)
			s.config.Ports = []string{addr}
			s.config.KeepAlive = tc.idle
			s.config.Nodelay = tc.nodelay
			g := s.newGen(ctx)
			s.forwarder(g).run()
			user := dialRetry(t, addr)
			defer user.Close()

			var local LocalTCPConn
			eventually(t, time.Second, func() bool {
				var ok bool
				local, ok = g.localChannel.pop()
				return ok
			}, "forwarded connection was not queued")
			defer local.conn.Close()
			if !local.pairing.claim() {
				t.Fatal("forwarded connection expired before socket inspection")
			}
			raw, err := local.conn.(*net.TCPConn).SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			wantNodelay := 0
			if tc.nodelay {
				wantNodelay = 1
			}
			if err := raw.Control(func(fd uintptr) {
				for _, opt := range []struct{ level, name, want int }{
					{unix.SOL_SOCKET, unix.SO_KEEPALIVE, 1},
					{unix.IPPROTO_TCP, unix.TCP_KEEPIDLE, tc.wantIdle},
					{unix.IPPROTO_TCP, unix.TCP_NODELAY, wantNodelay},
				} {
					got, err := unix.GetsockoptInt(int(fd), opt.level, opt.name)
					if err != nil || got != opt.want {
						t.Errorf("socket option %d/%d = %d, %v; want %d", opt.level, opt.name, got, err, opt.want)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
