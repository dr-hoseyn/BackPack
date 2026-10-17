package manage

import (
	"strings"
	"testing"

	"github.com/backpack/backpack/internal/manage/tunnelspec"
	"github.com/backpack/backpack/internal/tui"
)

func TestSafeReviewNeverConfirmsOnBlankOrEOF(t *testing.T) {
	preview, err := tunnelspec.PreviewChange([]byte("[l3]\nmtu=1400\ntoken='before-secret'\n"), []byte("[l3]\nmtu=1300\ntoken='after-secret'\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "\n", "n\n"} {
		restore := tui.SetInput(strings.NewReader(input))
		confirmed := false
		out := captureForwardPortScreen(t, func() { confirmed = confirmConfigChanges(preview) })
		restore()
		if confirmed || strings.Contains(out, "before-secret") || strings.Contains(out, "after-secret") {
			t.Fatalf("unsafe confirmation or secret disclosure: %s", out)
		}
		if !strings.Contains(out, "1400") || !strings.Contains(out, "1300") {
			t.Error("preview omitted the old/new values")
		}
	}
}

func TestSafePortClearingIsExplicitAndKeepsRelay(t *testing.T) {
	relay := "19000=127.0.0.1:1080"
	s := TunnelSpec{Role: "server", BindAddr: "0.0.0.0:9000", Ports: []string{"443", relay}}
	if changed, err := (TunnelEdit{}).editPorts(&s); changed || err != nil || len(s.Ports) != 2 {
		t.Fatal("blank form changed the ports")
	}
	if changed, err := (TunnelEdit{ClearPorts: true}).editPorts(&s); !changed || err != nil || len(s.Ports) != 1 || s.Ports[0] != relay {
		t.Fatalf("explicit clear: %+v err=%v", s.Ports, err)
	}
}

func TestSafeTLSPlanningDoesNotGenerateCertificates(t *testing.T) {
	previous := EnsureSelfSignedCert
	EnsureSelfSignedCert = func(string, string) (string, string, error) {
		t.Fatal("planning generated a certificate")
		return "", "", nil
	}
	defer func() { EnsureSelfSignedCert = previous }()
	s := TunnelSpec{Name: "safe-preview-test", Role: "server", Transport: "tcp"}
	if err := switchTransport(&s, "wss"); err != nil {
		t.Fatal(err)
	}
	cert, key := selfSignedCertPaths(s.Name)
	if s.TLSCert != cert || s.TLSKey != key {
		t.Fatal("preview did not plan the certificate paths")
	}
	domain := "preview.example.com"
	s.TLSCert, s.TLSKey = "", ""
	if _, err := (TunnelEdit{ACMEDomain: &domain}).editCertificate(&s); err != nil {
		t.Fatal(err)
	}
}
