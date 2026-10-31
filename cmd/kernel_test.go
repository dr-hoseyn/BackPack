package cmd

import (
	"strings"
	"testing"

	"github.com/backpack/backpack/config"
)

func TestKernelConfigurationSelectsAnExclusiveEngine(t *testing.T) {
	c := config.Config{Kernel: config.KernelConfig{Method: "gre", Role: "iran", LocalAddr: "192.0.2.1", RemoteAddr: "192.0.2.2", LocalIP: "10.77.1.1/30", PeerIP: "10.77.1.2", Token: strings.Repeat("k", 48)}}
	if err := checkEngine(&c); err != nil {
		t.Fatal(err)
	}
	for _, mixed := range []func(*config.Config){func(c *config.Config) { c.L3.Mode = "dial" }, func(c *config.Config) { c.Server.BindAddr = "0.0.0.0:8080" }, func(c *config.Config) { c.Client.RemoteAddr = "192.0.2.3:8080" }} {
		other := c
		mixed(&other)
		if err := checkEngine(&other); err == nil {
			t.Fatal("a mixed kernel configuration reached another engine")
		}
	}
	role, transport := engineIdentity(&c)
	if role != "iran" || transport != "kernel-gre" {
		t.Fatalf("wrong metrics identity %s/%s", role, transport)
	}
}
