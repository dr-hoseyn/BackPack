package manage

import (
	"encoding/pem"
	"github.com/backpack/backpack/config"
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
