package manage

import (
	"errors"
	"testing"

	"github.com/backpack/backpack/config"
)

func TestManagedAndDirectProtocolLabelsPreserveEngineTransports(t *testing.T) {
	previous := LoadSpec
	defer func() { LoadSpec = previous }()
	for _, role := range []string{"server", "client"} {
		for _, mode := range []string{"naive", "xhttp", "reality"} {
			spec := TunnelSpec{Role: role, Transport: "tcp"}
			if mode == "naive" {
				if role == "server" {
					spec.NaiveServer = config.NaiveServerConfig{Listen: ":443"}
				} else {
					spec.NaiveClient = config.NaiveClientConfig{Server: "example.com:443"}
				}
			} else if role == "server" {
				spec.XrayServer = config.XrayServerConfig{Mode: mode}
			} else {
				spec.XrayClient = config.XrayClientConfig{Mode: mode}
			}
			LoadSpec = func(string) (TunnelSpec, error) { return spec, nil }
			tunnel := Tunnel{Name: "wrapped", Role: role, Transport: "tcp"}
			want := map[string]string{"naive": "Naive / HTTP2", "xhttp": "XHTTP / TLS", "reality": "REALITY / Vision"}[mode]
			if got := TunnelTransportLabel(tunnel); got != want || tunnel.Transport != "tcp" {
				t.Fatalf("%s/%s: label %q, engine transport %s", role, mode, got, tunnel.Transport)
			}
		}
	}
	LoadSpec = func(string) (TunnelSpec, error) { return TunnelSpec{}, errors.New("no config") }
	for carrier, want := range map[string]string{
		"masque": "Direct MASQUE (HTTP/3)", "sni": "Direct SNI Spoofing", "xdi": "Direct xDi", "pck": "Direct PCK", "quic": "Direct Quic",
	} {
		if got := TunnelTransportLabel(Tunnel{Transport: "l3/" + carrier}); got != want {
			t.Fatalf("%s: %q", carrier, got)
		}
	}
	if got := TunnelTransportLabel(Tunnel{Transport: "tcp"}); got != "TCP" {
		t.Fatalf("missing config lost fallback: %q", got)
	}
}
