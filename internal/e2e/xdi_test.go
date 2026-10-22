package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/server"
)

// Run explicitly in an isolated Linux namespace with CAP_NET_RAW. Ordinary
// unprivileged tests still cover ICMP session demultiplexing in memory.
func TestXDIReverseRealICMPConcurrentTrafficAndRestarts(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("BACKPACK_XDI_LIVE") != "1" {
		t.Skip("set BACKPACK_XDI_LIVE=1 in a privileged Linux network namespace")
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			backend := startEchoBackend(t)
			udpBackend := startUDPEcho(t)
			type peer struct {
				s          *config.ServerConfig
				c          *config.ClientConfig
				tun        *tunnel
				stopServer func()
				udpEntry   string
				entry      int
			}
			var peers []peer
			for i := 0; i < 2; i++ {
				port, entry, udpEntry := freePort(t), freePort(t), freePort(t)
				token := fmt.Sprintf("xdi-reverse-%s-%d-0123456789abcdef", host, i)
				s := baseServerConfig("xdi", port, entry, backend.addr, token)
				s.BindAddr = net.JoinHostPort(host, fmt.Sprint(port))
				s.Ports = append(s.Ports, fmt.Sprintf("127.0.0.1:%d=%s", udpEntry, udpBackend))
				on := true
				s.AcceptUDP = &on
				c := baseClientConfig("xdi", s.BindAddr, token, nil)
				stop := startXDIReverseServer(t, s)
				tun := startXDIReverseClient(t, c, entry, port)
				if err := tun.waitReady(30 * time.Second); err != nil {
					t.Fatalf("peer %d never carried traffic over real ICMP: %v", i, err)
				}
				peers = append(peers, peer{s, c, tun, stop, fmt.Sprintf("127.0.0.1:%d", udpEntry), entry})
			}
			check := func() {
				t.Helper()
				for i, p := range peers {
					if err := concurrentEchoes(p.tun.Entry, 12, 64<<10); err != nil {
						t.Fatalf("peer %d concurrent TCP streams: %v", i, err)
					}
					if err := udpRoundTrip(p.udpEntry, randomPayload(t, 1024)); err != nil {
						t.Fatalf("peer %d forwarded UDP: %v", i, err)
					}
				}
			}
			check()
			// Restart one client while the other tunnel stays up. New pool
			// sessions must not collide with either existing control channel.
			p := &peers[0]
			p.tun.Stop()
			p.tun = startXDIReverseClient(t, p.c, p.entry, p.tun.TunnelPort)
			if err := p.tun.waitReady(45 * time.Second); err != nil {
				t.Fatalf("client restart did not recover: %v", err)
			}
			check()
			// The surviving client must reconnect after the Iran endpoint is
			// restarted, without restarting that client or the other tunnel.
			p.stopServer()
			waitPortFree(t, p.entry, 15*time.Second)
			p.stopServer = startXDIReverseServer(t, p.s)
			if err := p.tun.waitReady(60 * time.Second); err != nil {
				t.Fatalf("server restart did not recover: %v", err)
			}
			check()
		})
	}
}

func startXDIReverseServer(t *testing.T, cfg *config.ServerConfig) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := server.NewServer(cfg, ctx)
	done := make(chan struct{})
	go func() { defer close(done); srv.Start() }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); srv.Stop(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func startXDIReverseClient(t *testing.T, cfg *config.ClientConfig, entry, port int) *tunnel {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tun := startClientAgainst(t, ctx, cfg, entry, port)
	tun.cancel = cancel
	t.Cleanup(tun.Stop)
	return tun
}
