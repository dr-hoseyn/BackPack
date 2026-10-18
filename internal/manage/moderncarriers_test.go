package manage

import (
	"fmt"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
)

func TestModernCarrierBundlePairsAndPreservesIndependentIdentities(t *testing.T) {
	bin, cert, key := managedWizardFixture(t)
	carriers := []string{"trusttunnel", "webtransport", "xhttp-reality", "tailcat"}
	endpoints := []string{}
	for i := range carriers {
		endpoints = append(endpoints, fmt.Sprintf("0.0.0.0:%d", 26000+i))
	}
	s, err := buildAutomaticSpec("modern-pair", "localhost", "127.0.0.1:23000", automaticTLSMaterial{Cert: cert, Key: key}, "cover.example:443", carriers, endpoints, []string{"27000=127.0.0.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.ServerPaths {
		p := &s.ServerPaths[i]
		if p.Xray.Enabled() {
			p.Xray.Binary = bin
		}
		if p.Sing.Enabled() {
			p.Sing.Binary = bin
			p.Sing.Tailcat.DERPMapURL = "https://relay.example/derpmap.json"
			p.Sing.Tailcat.DERPRegion = 901
		}
	}
	if err := validateManagedSpec(s); err != nil {
		t.Fatal(err)
	}
	raw := pendingReverseLink(s, "localhost", linkExtras{})
	if raw == "" {
		t.Fatal("pairing failed")
	}
	l, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := decodeManagedPaths(l)
	if err != nil {
		t.Fatal(err)
	}
	client := reverseClientFromLink(l, "localhost")
	if len(client.ClientPaths) != 4 {
		t.Fatal("carriers lost")
	}
	if sub[2].HelperPublicKey == "" || sub[2].HelperPath == "" || sub[2].HelperShortID == "" {
		t.Fatal("XHTTP REALITY pairing incomplete")
	}
	if sub[3].HelperTailcatKey != s.ServerPaths[3].Sing.Tailcat.ClientKey || sub[3].HelperTailcatKey == s.ServerPaths[3].Sing.Tailcat.PrivateKey {
		t.Fatal("Tailcat server key exported instead of client provisioning key")
	}
	if client.ClientPaths[3].Sing.Tailcat.PrivateKey != sub[3].HelperTailcatKey || !strings.Contains(client.ClientPaths[3].Sing.Binary, "1.15.0-alpha.12") {
		t.Fatal("Tailcat peer identity/helper lost")
	}
	for i, p := range client.ClientPaths {
		if p.Connect.Enabled() {
			if p.Connect.Protocol != carriers[i] || p.Connect.Username == "" || p.Connect.Password == "" || p.Connect.CAFile == "" {
				t.Fatal("HTTP carrier authentication or trust lost")
			}
		}
	}
	var decoded config.Config
	if _, err := toml.Decode(s.Render(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Server.Paths[3].Sing.Tailcat.PrivateKey != s.ServerPaths[3].Sing.Tailcat.PrivateKey {
		t.Fatal("nested Tailcat config lost in render")
	}
	if decoded.Server.Paths[0].Connect != s.ServerPaths[0].Connect {
		t.Fatal("HTTP carrier fields lost in render")
	}
}

func TestAutomaticModernSelectionIsBoundedAndCanRunWithoutHelpers(t *testing.T) {
	for _, available := range [][]string{automaticCarriers(true, true, true, "cover.example:443"), automaticCarriers(false, false, false, "")} {
		selected := modernAutomaticCarriers(available)
		if len(selected) < 2 || len(selected) > 8 {
			t.Fatal("unbounded/incomplete carrier pool")
		}
		for _, v := range selected {
			if v == "tailcat" {
				t.Fatal("experimental carrier enabled implicitly")
			}
		}
	}
	selected, err := customAutomaticCarriers("trusttunnel,webtransport", "", "", false, false, false)
	if err != nil || len(selected) != 2 {
		t.Fatal("built-in carriers incorrectly require helpers", err)
	}
	for _, raw := range []string{"trusttunnel,trusttunnel", "trusttunnel,", "trusttunnel,xhttp-reality", "trusttunnel,unknown"} {
		if _, err := customAutomaticCarriers(raw, "", "", false, false, false); err == nil {
			t.Fatalf("invalid combination accepted: %s", raw)
		}
	}
}
