package e2e

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// TestConnectionLimitIsEnforced checks that a connection cap actually refuses
// connections past the limit, and — just as important — that it does not refuse
// connections below it.
func TestConnectionLimitIsEnforced(t *testing.T) {
	backend := startEchoBackend(t)

	tunnelPort := freePort(t)
	entryPort := freePort(t)
	const token = "limits-token-0123456789abcdefgh"
	const cap = 3

	srvCfg := baseServerConfig("tcpmux", tunnelPort, entryPort, backend.addr, token)
	srvCfg.MaxConnections = cap
	cliCfg := baseClientConfig("tcpmux", fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil)

	tun := runPair(t, srvCfg, cliCfg, entryPort, tunnelPort)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatalf("tunnel never came up: %v", err)
	}

	// Hold connections open so they occupy slots, and see how many can carry
	// data at once.
	var wg sync.WaitGroup
	var succeeded atomic.Int32
	release := make(chan struct{})

	for i := 0; i < cap*3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", tun.Entry, 3*time.Second)
			if err != nil {
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(6 * time.Second))

			// A round trip proves the connection was really admitted, not just
			// accepted by the kernel and then dropped.
			if _, err := conn.Write([]byte("ping")); err != nil {
				return
			}
			buf := make([]byte, 4)
			if _, err := conn.Read(buf); err != nil {
				return
			}
			succeeded.Add(1)
			<-release // hold the slot
		}()
	}

	// Give every attempt time to resolve one way or the other.
	time.Sleep(4 * time.Second)
	admitted := succeeded.Load()
	close(release)
	wg.Wait()

	if admitted == 0 {
		t.Fatal("the connection cap refused everything — a limit must still allow traffic up to it")
	}
	if int(admitted) > cap {
		t.Fatalf("%d connections carried data at once with the cap set to %d", admitted, cap)
	}
	t.Logf("cap %d: %d connections admitted simultaneously", cap, admitted)
}

// TestNoLimitsByDefault guards the more dangerous failure: a limit appearing
// where the user never asked for one.
func TestNoLimitsByDefault(t *testing.T) {
	backend := startEchoBackend(t)
	tun := startTunnel(t, "tcpmux", backend, tunnelOptions{})

	const conns = 20
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tun.roundTrip(randomPayload(t, 2048)); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := failures.Load(); n > 0 {
		t.Fatalf("%d of %d connections failed with no limits configured", n, conns)
	}
}

func TestLegacyBandwidthKeyDoesNotPaceTraffic(t *testing.T) {
	backend := startEchoBackend(t)
	tunnelPort, entryPort := freePort(t), freePort(t)
	const token = "bwlimit-token-0123456789abcdefg"
	srvCfg := baseServerConfig("tcpmux", tunnelPort, entryPort, backend.addr, token)
	if _, err := toml.Decode("bandwidth_mbps = 1", srvCfg); err != nil {
		t.Fatalf("legacy config no longer decodes: %v", err)
	}
	cliCfg := baseClientConfig("tcpmux", fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil)
	tun := runPair(t, srvCfg, cliCfg, entryPort, tunnelPort)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatal(err)
	}
	// The former 1 Mbit/s cap took over 30 seconds for this echo.
	if err := tun.roundTrip(randomPayload(t, 2<<20)); err != nil {
		t.Fatalf("legacy bandwidth setting prevented an ordinary transfer: %v", err)
	}
}
