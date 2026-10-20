package manage

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/amneziawg"
)

func TestConnTestSetupSupportsEveryOfferedTestEngine(t *testing.T) {
	for _, tr := range connTestReverse {
		if !ctSetupSupported("reverse", tr) {
			t.Errorf("reverse %s missing", tr)
		}
	}
	for _, tr := range connTestDirect {
		if !ctSetupSupported("direct", tr) {
			t.Errorf("direct %s missing", tr)
		}
	}
	if !ctSetupSupported("awg", "awg") || ctSetupSupported("spoof", ctSpoofToIran) {
		t.Fatal("AWG is a tunnel; spoof capability probes are not")
	}
}

func TestConnTestPromotionRetainsPrivateTrustAfterTemporaryFilesAreGone(t *testing.T) {
	for _, tr := range connTestReverse {
		t.Run(tr, func(t *testing.T) {
			temporary := t.TempDir()
			cert, key, _, err := ctCertificate(temporary, "fixture", "conntest.backpack.invalid")
			if err != nil {
				t.Fatal(err)
			}
			oldToken := strings.Repeat("a", 32)
			server := config.Config{Server: config.ServerConfig{BindAddr: "127.0.0.1:45001", Transport: config.TCP, Token: oldToken, Ports: []string{"127.0.0.1:45002=127.0.0.1:45003"}}}
			client := config.Config{Client: config.ClientConfig{RemoteAddr: "192.0.2.1:45001", Transport: config.TCP, Token: oldToken}}
			server.Server.TLSCertFile, server.Server.TLSKeyFile = cert, key
			// Populate each real helper config family. Promotion must retain
			// identity/trust and rotate exactly the paired authentication fields.
			server.Server.Naive = config.NaiveServerConfig{Password: oldToken, Certificate: cert, Key: key}
			client.Client.Naive = config.NaiveClientConfig{Password: oldToken, CAFile: cert}
			server.Server.Xray = config.XrayServerConfig{Mode: "reality", UUID: ctUUID(oldToken), ShortID: oldToken[:16], PrivateKey: "host-private", Certificate: cert, Key: key}
			client.Client.Xray = config.XrayClientConfig{Mode: "reality", UUID: ctUUID(oldToken), ShortID: oldToken[:16], PublicKey: "host-public", CAFile: cert}
			server.Server.Sing = config.SingServerConfig{Protocol: tr, Password: oldToken, UUID: ctUUID(oldToken), ObfsPassword: oldToken, ShadowsocksKey: ctSetupAWGKey(oldToken), Certificate: cert, Key: key}
			client.Client.Sing = config.SingClientConfig{Protocol: tr, Password: oldToken, UUID: ctUUID(oldToken), ObfsPassword: oldToken, ShadowsocksKey: ctSetupAWGKey(oldToken), CAFile: cert}
			server.Server.Connect = config.ConnectServerConfig{Protocol: tr, Password: oldToken, Path: "/old", Certificate: cert, Key: key}
			client.Client.Connect = config.ConnectClientConfig{Protocol: tr, Password: oldToken, Path: "/old", CAFile: cert}
			forms := make([]PeerForm, 2)
			for i, side := range []string{"iran", "kharej"} {
				cfg := server
				if side == "kharej" {
					cfg = client
				}
				var body bytes.Buffer
				if err := toml.NewEncoder(&body).Encode(cfg); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(temporary, "case.toml"), body.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				m, err := ctCaptureSetupMaterial(ConnTestLink{Cases: []ShareLink{{Name: "case"}}}, temporary)
				if err != nil {
					t.Fatal(err)
				}
				m.root = t.TempDir()
				f := PeerForm{Kind: "reverse", Side: side, Name: "quick-" + side, Token: strings.Repeat("Z", 64), TestCase: "case", Ports: "443=127.0.0.1:2096"}
				if i == 1 {
					if err := os.RemoveAll(temporary); err != nil {
						t.Fatal(err)
					}
				}
				forms[i], err = m.prepare(f)
				if err != nil {
					t.Fatal(err)
				}
			}
			a, b := forms[0].SetupConfig.Server, forms[1].SetupConfig.Client
			if a.Token != b.Token || a.Token == oldToken || a.Sing.Password != b.Sing.Password || a.Xray.UUID != b.Xray.UUID || a.Xray.ShortID != b.Xray.ShortID {
				t.Fatal("paired authentication diverged")
			}
			if len(a.Xray.UUID) != 36 {
				t.Fatal("invalid permanent UUID")
			}
			if _, err := hex.DecodeString(a.Xray.ShortID); err != nil {
				t.Fatal("REALITY short ID is not hexadecimal")
			}
			if a.Xray.PrivateKey != "host-private" || b.Xray.PublicKey != "host-public" {
				t.Fatal("local identities changed unexpectedly")
			}
			for _, f := range forms {
				path := f.SetupConfig.Server.TLSCertFile
				if f.Side == "kharej" {
					path = f.SetupConfig.Client.Sing.CAFile
				}
				if strings.HasPrefix(path, temporary) {
					t.Fatal("permanent config still points at disposable trust")
				}
				if _, err := os.ReadFile(path); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(f)
				if strings.Contains(string(raw), "host-private") || strings.Contains(string(raw), "BEGIN PRIVATE KEY") || strings.Contains(string(raw), f.SetupAssetDir) {
					t.Fatal("local private configuration escaped onto the coordinator wire")
				}
				if err := ctRemoveSetupAssets(f); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestConnTestAWGPromotionKeepsDistinctPrivateKeysAndChangesSharedKeys(t *testing.T) {
	a, b, err := amneziawg.NewPair("192.0.2.1:25999", "10.254.248.1/32", "10.254.248.2/32")
	if err != nil {
		t.Fatal(err)
	}
	var promoted [2]PeerForm
	for i, side := range []string{"iran", "kharej"} {
		cfg := a
		if i == 1 {
			cfg = b
		}
		m := &ctSetupMaterial{cases: map[string]config.Config{"awg": {AWG: config.AWGConfig(cfg)}}, assets: map[string][]byte{}, root: t.TempDir()}
		promoted[i], err = m.prepare(PeerForm{Kind: "awg", Side: side, Name: "quick-awg-" + side, Token: strings.Repeat("Z", 64), TestCase: "awg", Ports: "443=127.0.0.1:2096"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := awgBody(promoted[i].SetupConfig.AWG); err != nil {
			t.Fatal(err)
		}
	}
	x, y := promoted[0].SetupConfig.AWG, promoted[1].SetupConfig.AWG
	if x.PrivateKey != a.PrivateKey || y.PrivateKey != b.PrivateKey || x.PrivateKey == y.PrivateKey {
		t.Fatal("AWG local private identities were not preserved")
	}
	if x.PresharedKey != y.PresharedKey || x.PresharedKey == a.PresharedKey || x.Obfuscation.HeaderProtectionKey != y.Obfuscation.HeaderProtectionKey {
		t.Fatal("AWG shared authentication is not freshly paired")
	}
	if x.Ports[0] != "443=10.254.248.2:2096" || len(y.Ports) != 0 {
		t.Fatalf("AWG forwarded mapping: %+v / %+v", x.Ports, y.Ports)
	}
	if _, err := ctAWGForwardPorts("443=203.0.113.8:2096", "10.254.248.2"); err == nil {
		t.Fatal("AWG would bypass its peer host route")
	}
}

func TestConnTestPromotionRefusesAnUnownedAssetDirectory(t *testing.T) {
	m := &ctSetupMaterial{root: t.TempDir(), cases: map[string]config.Config{"case": {Client: config.ClientConfig{RemoteAddr: "192.0.2.1:443"}}}}
	dir := filepath.Join(m.root, "quick-existing")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := m.prepare(PeerForm{Kind: "reverse", Side: "kharej", Name: "quick-existing", Token: "new", TestCase: "case"})
	if err == nil {
		t.Fatal("unowned asset directory was accepted")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "keep")); string(data) != "existing" {
		t.Fatal("existing assets were changed")
	}
}
