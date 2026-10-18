package manage

import (
	"strings"
	"testing"
)

func TestEconomyPairsReducedPolicy(t *testing.T) {
	binary, cert, key := managedWizardFixture(t)
	available := automaticCarriers(true, true, "cover.example:443")
	carriers := economicalCarriers(available)
	if len(carriers) != 3 || carriers[0] != "hysteria2" {
		t.Fatal("economic carrier diversity")
	}
	s, err := buildAutomaticSpec("economy-test", "localhost", "127.0.0.1:23000", automaticTLSMaterial{Cert: cert, Key: key}, "cover.example:443", carriers, []string{"0.0.0.0:24000", "0.0.0.0:24001", "0.0.0.0:24002"}, []string{"25000=127.0.0.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	s.PathProfile = "economy"
	for i := range s.ServerPaths {
		if s.ServerPaths[i].Sing.Enabled() {
			s.ServerPaths[i].Sing.Binary = binary
		}
		if s.ServerPaths[i].Xray.Enabled() {
			s.ServerPaths[i].Xray.Binary = binary
		}
	}
	raw := pendingReverseLink(s, "localhost", linkExtras{})
	if raw == "" {
		t.Fatal("no link")
	}
	l, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	if l.PathProfile != "economy" {
		t.Fatal("profile lost")
	}
	var client TunnelSpec
	managedClientFromLink(&client, l, "localhost")
	if client.PathPolicy.StandbyInterval != 60 || client.PathPolicy.ProbeBytes != 1024 || len(client.ClientPaths) != 3 {
		t.Fatal("economic policy not paired")
	}
	if !strings.Contains(s.Render(), `path_profile = "economy"`) {
		t.Fatal("server edits lose profile")
	}
}
