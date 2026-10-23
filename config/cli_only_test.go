package config

import (
	"reflect"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestLegacyWebKeysAreInertWhenOldConfigurationsAreLoaded(t *testing.T) {
	var cfg Config
	_, err := toml.Decode(`[server]
bind_addr = "127.0.0.1:3080"
transport = "tcp"
web_port = 7777
web_bind = "0.0.0.0"
`, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.BindAddr != "127.0.0.1:3080" {
		t.Fatal("legacy keys broke tunnel loading")
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(cfg.Server), reflect.TypeOf(cfg.Client)} {
		for _, field := range []string{"WebPort", "WebBind"} {
			if _, exists := typ.FieldByName(field); exists {
				t.Fatalf("retired setting %s is still active", field)
			}
		}
	}
}
