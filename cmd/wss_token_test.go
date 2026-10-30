package cmd

import (
	"github.com/backpack/backpack/config"
	"testing"
)

func TestWSSRefusesPublicCompatibilityToken(t *testing.T) {
	for _, side := range []string{"server", "client"} {
		for _, tr := range []config.TransportType{config.WSS, config.WSSMUX} {
			for _, token := range []string{"", defaultToken, "  "} {
				cfg := config.Config{}
				if side == "server" {
					cfg.Server = config.ServerConfig{BindAddr: "127.0.0.1:443", Transport: tr, Token: token}
				} else {
					cfg.Client = config.ClientConfig{RemoteAddr: "example.com:443", Transport: tr, Token: token}
				}
				if e := checkWSSPrivateToken(&cfg); e == nil {
					t.Fatalf("%s %s accepted public/empty token", side, tr)
				}
			}
		}
	}
	cfg := config.Config{Client: config.ClientConfig{RemoteAddr: "example.com:443", Transport: config.TCP, Token: defaultToken, FallbackTransports: []config.TransportType{config.WSSMUX}}}
	if e := checkWSSPrivateToken(&cfg); e == nil {
		t.Fatal("WSS fallback accepted public token")
	}
	cfg.Client.Token = "a-private-random-token-0123456789"
	if e := checkWSSPrivateToken(&cfg); e != nil {
		t.Fatal(e)
	}
	// An unused side and the existing plain-TCP compatibility path are not
	// active secure-WebSocket endpoints.
	cfg.Client.FallbackTransports = nil
	cfg.Client.Token = defaultToken
	if e := checkWSSPrivateToken(&cfg); e != nil {
		t.Fatal(e)
	}
	cfg = config.Config{Server: config.ServerConfig{Transport: config.WSSMUX, Token: defaultToken}}
	if e := checkWSSPrivateToken(&cfg); e != nil {
		t.Fatal("unused side rejected")
	}
}
