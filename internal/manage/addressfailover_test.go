package manage

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
)

func TestIPv6HealthFailoverSurvivesConfigRoundTrip(t *testing.T) {
	s := TunnelSpec{
		Name: "dualstack", Role: "client", Transport: "tcpmux", Token: "test",
		RemoteAddr: "192.0.2.1:443", FallbackAddrs: []string{"[2001:db8::1]:443"},
		LoadBalance: true,
	}
	if err := setHealthFailover(&s, true); err != nil {
		t.Fatal(err)
	}
	if s.LoadBalance || !s.HealthFailover {
		t.Fatal("health failover must replace load balancing")
	}
	rendered := s.Render()
	if !strings.Contains(rendered, "health_failover = true") {
		t.Fatal("rendered config does not enable health failover")
	}
	var cfg config.Config
	if _, err := toml.Decode(rendered, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Client.HealthFailover || cfg.Client.LoadBalance ||
		cfg.Client.RemoteAddr != s.RemoteAddr || len(cfg.Client.FallbackAddrs) != 1 ||
		cfg.Client.FallbackAddrs[0] != s.FallbackAddrs[0] {
		t.Fatalf("lost dual-stack failover settings: %+v", cfg.Client)
	}
}

func TestHealthFailoverRequiresClientWithBackup(t *testing.T) {
	for _, s := range []TunnelSpec{{Role: "server"}, {Role: "client"}} {
		if err := setHealthFailover(&s, true); err == nil {
			t.Fatalf("enabled failover without a usable client backup: %+v", s)
		}
	}
	s := TunnelSpec{Role: "client", HealthFailover: true}
	if err := setHealthFailover(&s, false); err != nil || s.HealthFailover {
		t.Fatalf("cannot disable failover after removing backups: %v", err)
	}
}

func TestAddressFailoverEditsUseTheSharedSafeApply(t *testing.T) {
	previous := LoadSpec
	defer func() { LoadSpec = previous }()
	LoadSpec = func(string) (TunnelSpec, error) {
		return TunnelSpec{Role: "client", RemoteAddr: "192.0.2.1:443", FallbackAddrs: []string{"[2001:db8::1]:443"}, HealthFailover: true, LoadBalance: true}, nil
	}
	called := false
	if err := SetFallbackAddrs("test", nil, func(s TunnelSpec) error {
		called = true
		if s.HealthFailover || s.LoadBalance || len(s.FallbackAddrs) != 0 {
			t.Fatalf("removed backups left automatic switching enabled: %+v", s)
		}
		return nil
	}); err != nil || !called {
		t.Fatalf("backup edit did not use safe apply: called=%v err=%v", called, err)
	}
	LoadSpec = func(string) (TunnelSpec, error) {
		return TunnelSpec{Role: "client", RemoteAddr: "192.0.2.1:443", FallbackAddrs: []string{"192.0.2.1"}}, nil
	}
	if err := SetHealthFailover("test", true, func(TunnelSpec) error {
		t.Fatal("primary duplicated as a backup was applied")
		return nil
	}); err == nil {
		t.Fatal("enabled automatic failover with no distinct backup")
	}
}
