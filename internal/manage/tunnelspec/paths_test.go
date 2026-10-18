package tunnelspec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/app"
)

func TestManagedPathsSurviveLoadEditAndNeverEnterGenericJSON(t *testing.T) {
	binary, certificate, key := naiveSpecFiles(t)
	for _, role := range []string{"server", "client"} {
		t.Run(role, func(t *testing.T) {
			s := Spec{Role: role, Transport: "tcp", Token: "path-secret-token", ConnectionPool: 8, KeepAlive: 15, Ports: []string{"62003=127.0.0.1:62004"}}
			if role == "server" {
				s.BindAddr = "127.0.0.1:62001"
				s.ServerPaths = []config.ManagedServerPath{
					{Name: "primary", Endpoint: "edge.example:443", Naive: config.NaiveServerConfig{Binary: binary, Listen: "0.0.0.0:62002", Username: "user", Password: "private-password", Certificate: certificate, Key: key}},
					{Name: "fallback", Short: config.ShortServerConfig{Listen: "0.0.0.0:62005", ServerName: "example.org", Certificate: certificate, Key: key}},
				}
			} else {
				s.RemoteAddr = "127.0.0.1:62001"
				s.PathPolicy = config.PathPolicy{ProbeBytes: 4096, Improvement: 35}
				s.ClientPaths = []config.ManagedClientPath{
					{Name: "primary", Naive: config.NaiveClientConfig{Binary: binary, Server: "edge.example:443", Username: "user", Password: "private-password", CAFile: certificate}},
					{Name: "fallback", Short: config.ShortClientConfig{Server: "127.0.0.1:62005", ServerName: "127.0.0.1", CAFile: certificate}},
				}
			}
			if err := s.validateHelpers(); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(app.ConfigDir, 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.CreateTemp(app.ConfigDir, "paths-edit-*.toml")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(f.Name())
			if _, err = f.WriteString(s.Render()); err != nil {
				t.Fatal(err)
			}
			f.Close()
			loaded, err := Load(strings.TrimSuffix(filepath.Base(f.Name()), ".toml"))
			if err != nil {
				t.Fatal(err)
			}
			loaded.KeepAlive = 45
			var cfg config.Config
			if _, err = toml.Decode(loaded.Render(), &cfg); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(loaded.ServerPaths, s.ServerPaths) || !reflect.DeepEqual(loaded.ClientPaths, s.ClientPaths) || loaded.PathPolicy != s.PathPolicy {
				t.Fatal("ordinary edit lost path configuration")
			}
			if err = loaded.validateHelpers(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(loaded)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "private-password") || strings.Contains(string(raw), certificate) || strings.Contains(string(raw), key) {
				t.Fatal("managed path secrets entered generic JSON")
			}
		})
	}
}
