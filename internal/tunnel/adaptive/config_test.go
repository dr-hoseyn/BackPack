package adaptive

import (
	"testing"

	"github.com/backpack/backpack/config"
)

func TestManagedPathConfigurationRejectsAmbiguousPairingAndUnsupportedCombinations(t *testing.T) {
	server := func() config.Config {
		off := false
		return config.Config{Server: config.ServerConfig{BindAddr: "127.0.0.1:3080", Transport: config.TCP, Token: "private-path-token-0123456789abcdef", AcceptUDP: &off, Paths: []config.ManagedServerPath{
			{Name: "primary", Short: config.ShortServerConfig{Listen: "127.0.0.1:443", ServerName: "first.example.com", ACMEDomain: "first.example.com"}},
			{Name: "backup", Short: config.ShortServerConfig{Listen: "127.0.0.2:443", ServerName: "second.example.com", ACMEDomain: "second.example.com"}},
		}}}
	}
	if err := Validate(ptrConfig(server())); err != nil {
		t.Fatal("valid independent listeners refused:", err)
	}
	for name, change := range map[string]func(*config.Config){
		"one path":                    func(c *config.Config) { c.Server.Paths = c.Server.Paths[:1] },
		"duplicate identity":          func(c *config.Config) { c.Server.Paths[1].Name = c.Server.Paths[0].Name },
		"unreportable identity":       func(c *config.Config) { c.Server.Paths[0].Name = "path\nforged log" },
		"overlapping listeners":       func(c *config.Config) { c.Server.Paths[1].Short.Listen = c.Server.Paths[0].Short.Listen },
		"wildcard exported address":   func(c *config.Config) { c.Server.Paths[0].Endpoint = "0.0.0.0:443" },
		"invalid exported port":       func(c *config.Config) { c.Server.Paths[0].Endpoint = "first.example.com:65536" },
		"URL exported as endpoint":    func(c *config.Config) { c.Server.Paths[0].Endpoint = "https://first.example.com:443" },
		"injected exported authority": func(c *config.Config) { c.Server.Paths[0].Endpoint = "name@first.example.com:443" },
		"MPTCP loopback illusion":     func(c *config.Config) { c.Server.MPTCP = true },
		"old fallback chain":          func(c *config.Config) { c.Server.FallbackTransports = []config.TransportType{config.TCPMUX} },
		"weak token":                  func(c *config.Config) { c.Server.Token = "backpack" },
		"native UDP without opt in":   func(c *config.Config) { c.Server.Paths[0].Sing.NativeUDP = true },
	} {
		t.Run(name, func(t *testing.T) {
			c := server()
			change(&c)
			if Validate(&c) == nil {
				t.Fatal("ambiguous or unsupported configuration accepted")
			}
		})
	}
	client := config.Config{Client: config.ClientConfig{RemoteAddr: "127.0.0.1:3080", Transport: config.TCP, Token: "private-path-token-0123456789abcdef", Paths: []config.ManagedClientPath{
		{Name: "primary", Short: config.ShortClientConfig{Server: "first.example.com:443"}},
		{Name: "backup", Short: config.ShortClientConfig{Server: "second.example.com:443"}},
	}}}
	if err := Validate(&client); err != nil {
		t.Fatal("valid paired endpoints refused:", err)
	}
	client.Client.Paths[1].Short.Server = client.Client.Paths[0].Short.Server
	if Validate(&client) == nil {
		t.Fatal("duplicate public paths accepted as independent")
	}
}

func ptrConfig(c config.Config) *config.Config { return &c }
