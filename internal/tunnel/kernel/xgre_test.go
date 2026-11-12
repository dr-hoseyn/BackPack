package kernel

import (
	"reflect"
	"strings"
	"testing"

	"github.com/backpack/backpack/config"
)

func TestXGREMatchesObservedNativeProfile(t *testing.T) {
	c := example("xgre")
	c.ID = 0xf1234567
	p, err := Build(c)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"link", "add", p.Config.Interface, "type", "gre", "local", c.LocalAddr, "remote", c.RemoteAddr, "key", "4045620583", "ttl", "255", "tos", "inherit", "nopmtudisc", "ignore-df"}
	if !reflect.DeepEqual(p.Steps[0].Args, want) {
		t.Fatalf("native profile = %v, want %v", p.Steps[0].Args, want)
	}
	if p.Config.MTU != 1472 || p.Config.GREIgnoreDF == nil || !*p.Config.GREIgnoreDF {
		t.Fatal("observed Iran defaults changed")
	}
	peer, err := Build(Peer(p.Config))
	if err != nil {
		t.Fatal(err)
	}
	if peer.Config.ID != c.ID || peer.Config.MTU != 1472 || peer.Config.GREIgnoreDF == nil || *peer.Config.GREIgnoreDF {
		t.Fatal("paired key, MTU or local DF policy diverged")
	}
	for _, arg := range peer.Steps[0].Args {
		if arg == "ignore-df" {
			t.Fatal("Iran-only default leaked into peer")
		}
	}
}

func TestXGREPreservesAnExplicitLocalDFOverride(t *testing.T) {
	c := example("xgre")
	off := false
	c.GREIgnoreDF = &off
	p, err := Build(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range p.Steps[0].Args {
		if arg == "ignore-df" {
			t.Fatal("explicit local DF override ignored")
		}
	}
}

func TestXGREFullWidthKeyDoesNotRelaxOtherMethods(t *testing.T) {
	c := example("gre")
	c.ID = 0xf1234567
	if _, err := Resolve(c); err == nil {
		t.Fatal("ordinary GRE's existing ID range changed")
	}
	c = example("gre")
	on := true
	c.GREIgnoreDF = &on
	if _, err := Resolve(c); err == nil {
		t.Fatal("xgre-only setting silently ignored")
	}
	c = example("xgre")
	c.Lanes = 2
	if _, err := Resolve(c); err == nil {
		t.Fatal("xgre allowed unverified parallel lanes")
	}
	for _, addr := range []string{"2001:db8::1", "bad;command"} {
		c = example("xgre")
		c.LocalAddr = addr
		if _, err := Resolve(c); err == nil {
			t.Fatalf("accepted incompatible underlay %s", addr)
		}
	}
}

func TestXGREUnderlayDoesNotCreateAForwardOrDefaultRoute(t *testing.T) {
	p, err := Build(example("xgre"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Config.Ports) != 0 {
		t.Fatal("an underlay unexpectedly forwards user ports")
	}
	for _, step := range p.Steps {
		if strings.Contains(strings.Join(step.Args, " "), "default") {
			t.Fatal("changed host routing policy")
		}
	}
	if _, err := Forwarder(config.KernelConfig{PeerIP: p.Config.PeerIP}); err != nil {
		t.Fatal(err)
	}
}
