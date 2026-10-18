package manage

import (
	"encoding/pem"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tui"
	"strings"
	"testing"
)

func TestSingAdvancedSetupLinkCarriesPublicECHAndRealmWithoutPrivateKey(t *testing.T) {
	binary, cert, key := managedWizardFixture(t)
	public := string(pem.EncodeToMemory(&pem.Block{Type: "ECH CONFIGS", Bytes: []byte("public-configuration-sentinel")}))
	private := string(pem.EncodeToMemory(&pem.Block{Type: "ECH KEYS", Bytes: []byte("private-key-must-stay-on-iran")}))
	x := config.SingServerConfig{Binary: binary, Protocol: "hysteria2", Listen: "0.0.0.0:24000", ServerName: "localhost", Password: "independent-helper-password", Certificate: cert, Key: key, ObfsType: "gecko", ObfsPassword: "independent-gecko-password", ECHConfig: public, ECHKey: private, Realm: config.HysteriaRealm{ServerURL: "https://realm.example", Token: "independent-realm-token", RealmID: "pair-one", STUNServers: "stun.example:3478"}}
	s := TunnelSpec{Role: "server", Name: "modern-pair", Transport: "tcp", BindAddr: "127.0.0.1:23000", Token: strings.Repeat("s", 64), SingServer: x}
	raw := pendingReverseLink(s, "192.0.2.1", linkExtras{})
	if raw == "" {
		t.Fatal("no link")
	}
	l, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	if l.HelperECH != public || l.HelperRealmToken != x.Realm.Token || l.HelperObfsType != "gecko" {
		t.Fatal("lost advanced pairing settings")
	}
	encoded, err := l.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeShareLink(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var client TunnelSpec
	managedClientFromLink(&client, decoded, "192.0.2.1")
	if client.SingClient.ECHConfig != public || client.SingClient.Realm != x.Realm || client.SingClient.ObfsType != "gecko" {
		t.Fatal("wrong peer configuration")
	}
	if strings.Contains(client.Render(), private) || strings.Contains(encoded, private) {
		t.Fatal("private ECH key escaped")
	}
}

func TestSingAdvancedWizardCanChangeGeckoToSalamander(t *testing.T) {
	old := TunnelSpec{Role: "client", SingClient: config.SingClientConfig{Protocol: "hysteria2", ObfsPassword: "existing-obfuscation-secret", ObfsType: "gecko", GeckoMin: 600, GeckoMax: 1250}}
	var next = old
	restore := tui.SetInput(strings.NewReader("\n\n\n\nsalamander\n\nn\n\nn\n"))
	defer restore()
	ok := false
	capture(t, func() { ok = configureSing(&next, old, "hysteria2", "peer.example:443", "peer.example") })
	if !ok || next.SingClient.ObfsType != "salamander" || next.SingClient.GeckoMin != 0 || next.SingClient.GeckoMax != 0 || next.SingClient.ObfsPassword != old.SingClient.ObfsPassword {
		t.Fatal("changing obfuscation kept incompatible Gecko bounds or lost the account secret")
	}
}
