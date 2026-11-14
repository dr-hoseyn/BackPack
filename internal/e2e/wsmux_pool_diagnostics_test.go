package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
)

func TestWSMuxPoolDiagnosticsCarryTrafficAndPreserveQuietUsers(t *testing.T) {
	for _, mode := range []string{"default", "diagnostics", "conservative"} {
		t.Run(mode, func(t *testing.T) {
			metrics.ClearPool()
			t.Cleanup(metrics.ClearPool)
			backend := startEchoBackend(t)
			tunnelPort, entryPort := freePort(t), freePort(t)
			const token = "mux-pool-diagnostics-private-token-012345"
			srv := baseServerConfig("wssmux", tunnelPort, entryPort, backend.addr, token)
			cli := baseClientConfig("wssmux", fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil)
			srv.MuxVersion, cli.MuxVersion = 2, 2
			srv.MuxCon, cli.ConnectionPool, cli.WSMuxMaxSessions = 32, 4, 4
			cli.WSMuxPoolDiagnostics = mode == "diagnostics"
			cli.WSMuxConservativePool = mode == "conservative"
			tun := runPair(t, srv, cli, entryPort, tunnelPort)
			if err := tun.waitReady(tunnelReadyTimeout); err != nil {
				t.Fatal(err)
			}
			quiet, err := net.DialTimeout("tcp", tun.Entry, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer quiet.Close()
			check := func() {
				t.Helper()
				payload := []byte("quiet user survived pool sampling")
				quiet.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := quiet.Write(payload); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(payload))
				if _, err := io.ReadFull(quiet, got); err != nil || !bytes.Equal(got, payload) {
					t.Fatalf("quiet user: %q, %v", got, err)
				}
				quiet.SetDeadline(time.Time{})
			}
			check()
			var workers sync.WaitGroup
			errors := make(chan error, 8)
			payload := randomPayload(t, 128<<10)
			for range 8 {
				workers.Go(func() { errors <- tun.roundTrip(payload) })
			}
			workers.Wait()
			for range 8 {
				if err := <-errors; err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				p := (&metrics.Collector{}).Snapshot().Pool
				if p != nil && p.Mux != nil && p.Mux.Sessions == 4 && p.Mux.Streams == 1 {
					m := p.Mux
					if m.ActiveSessions != 1 || m.IdleSessions != 3 {
						t.Fatalf("incorrect occupancy: %+v", m)
					}
					if mode == "default" && m.PressureObserved != 0 {
						t.Fatalf("default performed optional pressure sampling: %+v", m)
					}
					if mode != "default" && runtime.GOOS == "linux" && m.PressureObserved != 4 {
						t.Fatalf("TCP observations did not reach snapshot: %+v", m)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("mux occupancy never reached snapshot: %+v", p)
				}
				time.Sleep(20 * time.Millisecond)
			}
			check()
		})
	}
}
