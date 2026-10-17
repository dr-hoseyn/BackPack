package manage

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestConnectionTestMasqueTrustIsPublicBoundedAndFrozen(t *testing.T) {
	dir := t.TempDir()
	_, key, ca, err := ctCertificate(dir, "masque", "conntest.backpack.invalid")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(ctCarrierSettings{TargetPort: 62345, ServerName: "conntest.backpack.invalid", CA: ca})
	raw := gzipB64(body)
	c := &ctCoordinator{tok: "test-secret"}
	if got := c.answer("masque-cert wrong "+raw, "::1"); got != "" {
		t.Fatal("unauthenticated identity accepted")
	}
	if got := c.answer("masque-cert test-secret "+raw, "::1"); got != "ok" {
		t.Fatalf("valid identity rejected: %q", got)
	}
	cfg, err := ctMasqueClient(c.masqueSettings, dir, "dial")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CertFile != "" || cfg.KeyFile != "" || cfg.Target != "127.0.0.1:62345" {
		t.Fatalf("client received wrong settings: %+v", cfg)
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil || string(pem) != ca {
		t.Fatal("trust anchor was not preserved")
	}
	secret, _ := os.ReadFile(key)
	if strings.Contains(string(body), string(secret)) || strings.Contains(string(body), "PRIVATE KEY") {
		t.Fatal("private key exported")
	}
	c.peer = "::1"
	if got := c.answer("masque-cert test-secret "+raw, "::1"); got != "" {
		t.Fatal("identity mutated after join")
	}
	for _, bad := range []string{"", strings.Repeat("A", 1801), gzipB64([]byte(`{"p":1,"s":"elsewhere","c":"invalid"}`))} {
		if _, err := ctDecodeMasqueSettings(bad); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
}
