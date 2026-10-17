package amneziawg

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testPair(t *testing.T) (Config, Config) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pair")
	if err := initialize("127.0.0.1:25999", dir, "10.254.248.1/32", "10.254.248.2/32", io.Discard); err != nil {
		t.Fatal(err)
	}
	a, err := Load(filepath.Join(dir, "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(filepath.Join(dir, "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestAmneziaWGPairHasIndependentIdentitiesAndBoundedRoutes(t *testing.T) {
	a, b := testPair(t)
	if a.PrivateKey == b.PrivateKey || a.PresharedKey != b.PresharedKey || a.Obfuscation != b.Obfuscation || a.Address != b.PeerAddress || b.Address != a.PeerAddress {
		t.Fatal("incorrect pairing")
	}
	for _, c := range []Config{a, b} {
		ipc, err := c.ipcConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ipc, "allowed_ip="+c.PeerAddress+"\n") || strings.Contains(ipc, "0.0.0.0/0") || strings.Contains(ipc, "::/0") {
			t.Fatal("configuration added a broad route")
		}
	}
}

func TestAmneziaWGRejectsDangerousAndIncompatibleConfiguration(t *testing.T) {
	c, _ := testPair(t)
	for name, change := range map[string]func(*Config){
		"existing system interface":    func(c *Config) { c.Interface = "eth0" },
		"oversized name":               func(c *Config) { c.Interface = "bpawg012345678901" },
		"prefix route":                 func(c *Config) { c.PeerAddress = "0.0.0.0/0" },
		"same address":                 func(c *Config) { c.PeerAddress = c.Address },
		"family mismatch":              func(c *Config) { c.PeerAddress = "fd00::2/128" },
		"low MTU":                      func(c *Config) { c.MTU = 1000 },
		"padding overflow":             func(c *Config) { c.MTU = 1380; c.Obfuscation.S4 = 64; c.Obfuscation.ContentPadding = 32 },
		"secret injection":             func(c *Config) { c.PrivateKey += "\nlisten_port=1" },
		"invalid peer key":             func(c *Config) { c.PeerPublicKey = "bad" },
		"bad psk":                      func(c *Config) { c.PresharedKey = "bad" },
		"bad header key":               func(c *Config) { c.Obfuscation.HeaderProtectionKey = "bad" },
		"no header protection padding": func(c *Config) { c.Obfuscation.S3 = 0 },
		"overlapping headers":          func(c *Config) { c.Obfuscation.H2 = c.Obfuscation.H1 },
		"standard WG header":           func(c *Config) { c.Obfuscation.H1 = "1-9" },
		"reverse header range":         func(c *Config) { c.Obfuscation.H1 = "99-5" },
		"unbounded junk":               func(c *Config) { c.Obfuscation.JunkCount = 100000 },
		"junk fragmentation":           func(c *Config) { c.Obfuscation.JunkMax = 65535 },
		"missing listener":             func(c *Config) { c.ListenPort = 0 },
		"name endpoint":                func(c *Config) { c.Endpoint = "example.com:443" },
		"unspecified endpoint":         func(c *Config) { c.Endpoint = "0.0.0.0:443" },
		"route loop":                   func(c *Config) { c.Endpoint = "10.254.248.2:443" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := c
			change(&bad)
			if bad.Validate() == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}

}

func TestAmneziaWGCommandsProtectSecretsAndExistingPair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pair")
	var out bytes.Buffer
	args := []string{"init", "--endpoint", "127.0.0.1:25999", "--dir", dir}
	if err := Run(context.Background(), args, &out); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "server.json")
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), args, &out); err == nil {
		t.Fatal("overwrote existing pair")
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("existing keys changed")
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.PrivateKey, c.PresharedKey, c.Obfuscation.HeaderProtectionKey} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("command printed secret")
		}
	}
	if err := Run(context.Background(), []string{"check", "--config", p}, &out); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"bad"}, {"check"}, {"check", "extra"}, {"init", "--endpoint", "bad", "--dir", filepath.Join(t.TempDir(), "nope")}} {
		if err := Run(context.Background(), args, io.Discard); err == nil {
			t.Fatal("accepted invalid command")
		}
	}
	if err := Run(context.Background(), []string{"help"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatal("accepted public private keys")
		}
	}
}

func TestAmneziaWGStrictJSON(t *testing.T) {
	c, _ := testPair(t)
	b, _ := json.Marshal(c)
	for _, body := range [][]byte{append(append([]byte(nil), b...), []byte("{}")...), []byte(`{"private_key":"secret","unknown":true}`), bytes.Repeat([]byte("x"), 17<<10)} {
		p := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(p, body, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatal("accepted invalid JSON")
		}
	}
}
