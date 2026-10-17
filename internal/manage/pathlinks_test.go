package manage

import (
	"os"
	"strings"
	"testing"

	"github.com/backpack/backpack/config"
)

func TestManagedPathSetupLinkPairsAllEndpointsWithoutExportingPrivateFiles(t *testing.T) {
	_, cert, key := managedWizardFixture(t)
	cfg := config.Config{Server: config.ServerConfig{BindAddr: "127.0.0.1:3080", Transport: config.TCP, Token: "reverse-pairing-token-0123456789abcdef", Paths: []config.ManagedServerPath{
		{Name: "short-a", Endpoint: "first.example:443", Short: config.ShortServerConfig{Listen: "0.0.0.0:8443", ServerName: "localhost", Certificate: cert, Key: key}},
		{Name: "short-b", Endpoint: "[2001:db8::5]:9443", Short: config.ShortServerConfig{Listen: "0.0.0.0:9443", ServerName: "localhost", Certificate: cert, Key: key}},
	}}}
	raw, err := shareLinkOf("paired", "198.51.100.1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	link, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := decodeManagedPaths(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 2 || sub[0].Host != "first.example" || sub[0].Port != "443" || sub[1].Host != "2001:db8::5" {
		t.Fatal("public endpoint override was lost")
	}
	private, _ := os.ReadFile(key)
	for _, p := range sub {
		if strings.Contains(p.HelperCA, "PRIVATE") || strings.Contains(p.HelperCA, string(private)) {
			t.Fatal("private key exported")
		}
		if p.HelperSNI != "localhost" || p.HelperCA == "" || p.Tok != cfg.Server.Token || p.InnerPort != "3080" {
			t.Fatal("paired trust or target lost")
		}
	}
	s := TunnelSpec{Role: "client", Token: link.Tok}
	managedClientFromLink(&s, link, link.Host)
	if s.RemoteAddr != "127.0.0.1:3080" || len(s.ClientPaths) != 2 || s.ClientPaths[0].Short.Server != "first.example:443" || s.ClientPaths[1].Short.Server != "[2001:db8::5]:9443" {
		t.Fatal("client pairing changed path endpoint")
	}
	bad := link
	wrong := sub[0]
	wrong.Tok = "different-secret"
	encoded, err := wrong.Encode()
	if err != nil {
		t.Fatal(err)
	}
	bad.PathLinks = append([]string(nil), link.PathLinks...)
	bad.PathLinks[0] = encoded
	if _, err = decodeManagedPaths(bad); err == nil {
		t.Fatal("different per-path reverse token accepted")
	}
	wrong = sub[0]
	wrong.Tr = "managed-paths"
	wrong.PathLinks = link.PathLinks
	encoded, err = wrong.Encode()
	if err != nil {
		t.Fatal(err)
	}
	bad.PathLinks[0] = encoded
	if _, err = decodeManagedPaths(bad); err == nil {
		t.Fatal("nested adaptive path link accepted")
	}
}

func TestManagedPathWizardKeepsGeneratedPrivateMaterialDistinctForLongParentNames(t *testing.T) {
	prefix := strings.Repeat("a", 35)
	first, second := managedPathChildName(prefix+"-one", 1), managedPathChildName(prefix+"-two", 1)
	if first == second || first == managedPathChildName(prefix+"-one", 2) {
		t.Fatal("independent paths share a generated certificate identity")
	}
	if len(first+"-https") > 40 || len(second+"-https") > 40 {
		t.Fatal("generated certificate name exceeds its budget")
	}
	if managedPathChildName(prefix+"-one", 1) != first {
		t.Fatal("same path does not retain its generated certificate identity")
	}
}
