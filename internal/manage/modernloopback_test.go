package manage

import (
	"net"
	"testing"

	"github.com/backpack/backpack/config"
)

func TestHTTPSetupLinksPreserveLiteralLoopbackTargets(t *testing.T) {
	_, cert, key := managedWizardFixture(t)
	for _, host := range []string{"127.0.0.1", "127.0.0.2", "::1"} {
		for _, bundle := range []bool{false, true} {
			kind := "single"
			if bundle {
				kind = "combined"
			}
			t.Run(net.JoinHostPort(host, "23000")+"/"+kind, func(t *testing.T) {
				s := config.ServerConfig{Transport: config.TCP, BindAddr: net.JoinHostPort(host, "23000"), Token: "private-engine-secret-123456789012"}
				s.Connect = config.ConnectServerConfig{Protocol: "trusttunnel", Mode: "auto", Listen: "0.0.0.0:26000", ServerName: "localhost", Username: "account", Password: "separate-secret-123456", Certificate: cert, Key: key}
				if bundle {
					wt := s.Connect
					wt.Protocol, wt.Mode, wt.Path, wt.Listen = "webtransport", "h3", "/private", "0.0.0.0:26001"
					s.Paths = []config.ManagedServerPath{{Name: "trusttunnel", Connect: s.Connect}, {Name: "webtransport", Connect: wt}}
					s.Connect = config.ConnectServerConfig{}
				}
				raw, err := shareLinkOf("loopback", "localhost", config.Config{Server: s})
				if err != nil {
					t.Fatal(err)
				}
				link, err := DecodeShareLink(raw)
				if err != nil {
					t.Fatal(err)
				}
				peer := reverseClientFromLink(link, "localhost")
				if peer.RemoteAddr != s.BindAddr {
					t.Fatalf("private target changed from %s to %s", s.BindAddr, peer.RemoteAddr)
				}
				if bundle {
					if _, err := decodeManagedPaths(link); err != nil {
						t.Fatal(err)
					}
					for i := range peer.ClientPaths {
						peer.ClientPaths[i].Connect.CAFile = cert
					}
					link.InnerHost = "192.0.2.1"
					if _, err := decodeManagedPaths(link); err == nil {
						t.Fatal("non-loopback private target accepted")
					}
				} else {
					peer.ConnectClient.CAFile = cert
				}
				if err := validateManagedSpec(peer); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
