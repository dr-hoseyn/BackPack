package e2e

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Diagnostic workload: distinguish cold admission from a fully warmed pool.
// This does not change sizing policy or infer a production capacity limit.
func TestWSSMuxColdBurstProfile(t *testing.T) {
	if os.Getenv("BP_WSSMUX_COLD_BURST") == "" {
		t.Skip("set BP_WSSMUX_COLD_BURST=1")
	}
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprintf("warm=%t", warm), func(t *testing.T) {
			backend := startEchoBackend(t)
			port, entry := freePort(t), freePort(t)
			const token = "wssmux-cold-burst-test-token-0123456789"
			srv := baseServerConfig("wssmux", port, entry, backend.addr, token)
			cli := baseClientConfig("wssmux", fmt.Sprintf("127.0.0.1:%d", port), token, nil)
			srv.MuxCon, cli.ConnectionPool = 512, 64
			srv.ChannelSize = 8192
			srv.MuxVersion, cli.MuxVersion = 2, 2
			cli.AggressivePool = false
			tun := runPair(t, srv, cli, entry, port)
			if warm {
				if err := tun.waitReady(tunnelReadyTimeout); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
			} else {
				// Start users as soon as the entry listener exists, without
				// waiting for the data-session handshakes to finish.
				deadline := time.Now().Add(tunnelReadyTimeout)
				for {
					conn, err := net.DialTimeout("tcp", tun.Entry, 100*time.Millisecond)
					if err == nil {
						conn.Close()
						break
					}
					if time.Now().After(deadline) {
						t.Fatal(err)
					}
					time.Sleep(time.Millisecond)
				}
			}
			const users = 4000
			conns := make([]net.Conn, users)
			t.Cleanup(func() {
				for _, c := range conns {
					if c != nil {
						c.Close()
					}
				}
			})
			start := make(chan struct{})
			failures := make(chan error, users)
			var workers sync.WaitGroup
			for i := range users {
				workers.Go(func() {
					<-start
					c, err := net.DialTimeout("tcp", tun.Entry, 10*time.Second)
					if err == nil {
						conns[i] = c
						_ = c.SetDeadline(time.Now().Add(15 * time.Second))
						_, err = c.Write([]byte{0x61})
						var got [1]byte
						if err == nil {
							_, err = io.ReadFull(c, got[:])
							if err == nil && got[0] != 0x61 {
								err = fmt.Errorf("corrupted echo")
							}
						}
					}
					if err != nil {
						failures <- err
					}
				})
			}
			began := time.Now()
			close(start)
			workers.Wait()
			elapsed := time.Since(began)
			// Allow any already-launched handshakes to settle before counting.
			time.Sleep(time.Second)
			data, err := os.ReadFile("/proc/net/tcp")
			if err != nil {
				t.Fatal(err)
			}
			sessions := 0
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) > 3 && fields[3] == "01" && strings.HasSuffix(fields[2], fmt.Sprintf(":%04X", port)) {
					sessions++
				}
			}
			t.Logf("COLD_BURST warm=%t users=%d configured_pool=64 mux_con=512 physical_sessions=%d errors=%d completion_ms=%.1f", warm, users, sessions-1, len(failures), float64(elapsed)/float64(time.Millisecond))
			if len(failures) > 0 {
				t.Errorf("burst failures: %d; first: %v", len(failures), <-failures)
			}
		})
	}
}
