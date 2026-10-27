package e2e

import (
	"fmt"
	"testing"
)

func TestWSMuxNegotiatedVersionsCarryRealTraffic(t *testing.T) {
	for _, transport := range []string{"wsmux", "wssmux"} {
		for _, versions := range [][2]int{{0, 0}, {0, 1}, {1, 0}, {2, 0}, {1, 1}, {2, 2}} {
			t.Run(fmt.Sprintf("%s/server%d-client%d", transport, versions[0], versions[1]), func(t *testing.T) {
				backend := startEchoBackend(t)
				tunnelPort, entryPort := freePort(t), freePort(t)
				const token = "wsmux-negotiation-token-0123456789"
				srv := baseServerConfig(transport, tunnelPort, entryPort, backend.addr, token)
				cli := baseClientConfig(transport, fmt.Sprintf("127.0.0.1:%d", tunnelPort), token, nil)
				srv.MuxVersion, cli.MuxVersion = versions[0], versions[1]
				tun := runPair(t, srv, cli, entryPort, tunnelPort)
				if err := tun.waitReady(tunnelReadyTimeout); err != nil {
					t.Fatal(err)
				}
				if err := tun.roundTrip(randomPayload(t, 256<<10)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
