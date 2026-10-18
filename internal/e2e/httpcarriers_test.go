package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

func TestModernReverseCarriersAndMixedPool(t *testing.T) {
	for _, protocol := range []string{"trusttunnel", "webtransport", "combined"} {
		t.Run(protocol, func(t *testing.T) {
			cert, key := testCert(t)
			backend := startEchoBackend(t)
			internal, entry, outer := freePort(t), freePort(t), freePort(t)
			s := baseServerConfig("tcp", internal, entry, backend.addr, "modern-reverse-e2e-token-0123456789abcdef")
			c := baseClientConfig("tcp", s.BindAddr, s.Token, nil)
			server := func(protocol string, port int) config.ConnectServerConfig {
				x := config.ConnectServerConfig{Protocol: protocol, Mode: "auto", Listen: fmt.Sprintf("127.0.0.1:%d", port), ServerName: "127.0.0.1", Username: "backpack", Password: "modern-e2e-carrier-account-secret", Certificate: cert, Key: key}
				if protocol == "webtransport" {
					x.Mode = "h3"
					x.Path = "/private-e2e-session"
				}
				return x
			}
			client := func(x config.ConnectServerConfig) config.ConnectClientConfig {
				return config.ConnectClientConfig{Protocol: x.Protocol, Mode: x.Mode, Server: x.Listen, ServerName: x.ServerName, Username: x.Username, Password: x.Password, Path: x.Path, CAFile: cert}
			}
			if protocol == "combined" {
				a, b := server("trusttunnel", outer), server("webtransport", freePort(t))
				s.Paths = []config.ManagedServerPath{{Name: "http-fallback", Connect: a}, {Name: "native-quic", Connect: b}}
				c.Paths = []config.ManagedClientPath{{Name: "http-fallback", Connect: client(a)}, {Name: "native-quic", Connect: client(b)}}
				c.PathPolicy = config.PathPolicy{ProbeInterval: 1, ProbeTimeout: 3, ProbeBytes: 1024, Failures: 1, Recoveries: 1}
			} else {
				s.Connect = server(protocol, outer)
				c.Connect = client(s.Connect)
			}
			tun := runPair(t, s, c, entry, internal)
			if err := tun.waitReady(20 * time.Second); err != nil {
				t.Fatal(err)
			}
			for range 5 {
				if err := tun.roundTrip([]byte("verified Reverse payload on " + protocol)); err != nil {
					t.Fatal(err)
				}
			}
			tun.Stop()
			waitPortFree(t, outer, 3*time.Second)
		})
	}
}
