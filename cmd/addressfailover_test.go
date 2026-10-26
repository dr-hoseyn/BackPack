package cmd

import (
	"testing"

	"github.com/backpack/backpack/config"
)

func TestFallbackConfigNormalizesIPv6AndRejectsMalformedBackups(t *testing.T) {
	cfg := config.Config{Client: config.ClientConfig{
		Transport: config.TCPMUX, RemoteAddr: "192.0.2.1:443",
		FallbackAddrs: []string{"2001:db8::1", "[2001:db8::1]:443", "192.0.2.1:443"},
	}}
	if err := checkFallbackChain(&cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Client.FallbackAddrs; len(got) != 1 || got[0] != "[2001:db8::1]:443" {
		t.Fatalf("backup configuration did not normalize: %v", got)
	}
	cfg.Client.FallbackAddrs = []string{"[2001:db8::1]:invalid"}
	if err := checkFallbackChain(&cfg); err == nil {
		t.Fatal("malformed backup reached the runtime")
	}
}
