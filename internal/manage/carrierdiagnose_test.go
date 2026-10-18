package manage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCarrierDiagnosticNeverExportsRawHelperLog(t *testing.T) {
	secret := "private-password-and-setup-link"
	for _, tc := range []struct{ log, code string }{
		{"authentication failed " + secret, "helper_reported_auth_failure"},
		{"timeout: no recent network activity " + secret, "helper_reported_quic_timeout"},
		{"certificate signed by unknown authority " + secret, "helper_reported_certificate_failure"},
		{"unknown error " + secret, "helper_route_unverified"},
	} {
		f, err := os.CreateTemp(t.TempDir(), "log")
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(tc.log)
		code, detail := helperDiagnosticSignal(f)
		f.Close()
		if code != tc.code || strings.Contains(detail, secret) {
			t.Fatalf("unsafe report: %s %s", code, detail)
		}
	}
}

func TestCarrierDiagnosticRejectsWrongRoleAndUnknownPathWithoutStartingHelpers(t *testing.T) {
	for _, data := range []string{
		"[server]\nbind_addr='127.0.0.1:3080'\n",
		"[client]\nremote_addr='127.0.0.1:3080'\ntransport='tcp'\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := DiagnoseDatagramConfig(t.Context(), path, "missing"); err == nil {
			t.Fatal("unsupported configuration accepted")
		}
	}
}
