package tunnelspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/app"
)

func TestWSSClientTrustSurvivesAnUnrelatedEdit(t *testing.T) {
	s := Spec{Role: "client", Transport: "wssmux", RemoteAddr: "example.com:443", Token: "token", ConnectionPool: 8,
		WSSCAFile: "/private/my CA.pem", WSSServerName: "origin.example.com", WSSAllowLegacyServer: true, WSSHTTP11Only: true, WSMuxMaxSessions: 16}
	if err := os.MkdirAll(app.ConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(app.ConfigDir, "wss-options-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(s.Render()); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	loaded, err := loadClient(strings.TrimSuffix(filepath.Base(f.Name()), ".toml"))
	if err != nil {
		t.Fatal(err)
	}
	loaded.KeepAlive = 60
	var cfg config.Config
	if _, err = toml.Decode(loaded.Render(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Client.WSSCAFile != s.WSSCAFile || cfg.Client.WSSServerName != s.WSSServerName || !cfg.Client.WSSAllowLegacyServer || !cfg.Client.WSSHTTP11Only || cfg.Client.WSMuxMaxSessions != 16 {
		t.Fatal("editing discarded client trust or session ceiling")
	}
}
