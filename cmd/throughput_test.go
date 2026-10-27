package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/enginectl"
)

func TestThroughputHostPolicySelectsTheActiveEngine(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  config.Config
		want bool
	}{
		{"server", config.Config{Server: config.ServerConfig{BindAddr: ":443", Preset: "throughput"}}, true},
		{"client", config.Config{Client: config.ClientConfig{RemoteAddr: "example.org:443", Preset: "throughput"}}, true},
		{"direct", config.Config{Direct: config.DirectConfig{Role: "iran", Preset: "throughput"}}, true},
		{"l3", config.Config{L3: config.L3Config{Mode: "listen", Preset: "throughput"}}, true},
		{"server opts out", config.Config{Server: config.ServerConfig{BindAddr: ":443", Preset: "throughput", SkipOptz: true}}, false},
		{"client opts out", config.Config{Client: config.ClientConfig{RemoteAddr: "example.org:443", Preset: "throughput", SkipOptz: true}}, false},
		{"server wins over client metadata", config.Config{Server: config.ServerConfig{BindAddr: ":443", Preset: "turbo"}, Client: config.ClientConfig{Preset: "throughput"}}, false},
		{"direct ignores reverse metadata", config.Config{Direct: config.DirectConfig{Role: "iran", Preset: "turbo"}, Server: config.ServerConfig{Preset: "throughput"}}, false},
		{"custom", config.Config{Server: config.ServerConfig{BindAddr: ":443"}}, false},
		{"turbo", config.Config{Server: config.ServerConfig{BindAddr: ":443", Preset: "turbo"}}, false},
		{"balance", config.Config{Server: config.ServerConfig{BindAddr: ":443", Preset: "balance"}}, false},
		{"aggressive", config.Config{Server: config.ServerConfig{BindAddr: ":443", Preset: "aggressive"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _ := throughputHostPolicy(&tt.cfg)
			if got != tt.want {
				t.Fatalf("selected = %v, want %v", got, tt.want)
			}
		})
	}
}

// A saved pre-update config gains host capacity without re-expanding the preset
// or rewriting operator-adjusted windows, pools, limits or the original file.
func TestThroughputExistingConfigAppliesCapacityAndPreservesManualTuning(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host capacity is Linux-only")
	}
	path := filepath.Join(t.TempDir(), "existing.toml")
	body := []byte(`[server]
bind_addr = "127.0.0.1:21000"
transport = "tcp"
token = "test-token"
preset = "throughput"
channel_size = 1234
mux_con = 37
mux_recievebuffer = 5242880
max_connections = 97
ports = ["22000-22002=127.0.0.1:62050"]
`)
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := loadFixture(t, path)
	before := *cfg
	old := enableThroughputCapacity
	t.Cleanup(func() { enableThroughputCapacity = old })
	calls := 0
	enableThroughputCapacity = func(ports []int) (string, error) {
		calls++
		for _, p := range []int{21000, 22000, 22001, 22002} {
			if !slices.Contains(ports, p) {
				t.Errorf("missing pre-listen reservation %d: %v", p, ports)
			}
		}
		return "", nil
	}
	applyThroughputHostTuning(cfg)
	if calls != 1 {
		t.Fatalf("capacity calls = %d", calls)
	}
	if !reflect.DeepEqual(*cfg, before) {
		t.Fatal("host tuning changed transport config")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, body) {
		t.Fatal("host tuning rewrote saved config", err)
	}
	// Failed tuning warns and keeps the usable tunnel config intact.
	enableThroughputCapacity = func([]int) (string, error) { return "", errors.New("kernel refused tuning") }
	applyThroughputHostTuning(cfg)
	if !reflect.DeepEqual(*cfg, before) {
		t.Fatal("failed tuning changed transport config")
	}
}

func TestThroughputReloadEnablesCapacityBeforeStartingTheNewGeneration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("host capacity is Linux-only")
	}
	path := filepath.Join(t.TempDir(), "reload.toml")
	oldDir := enginectl.Dir
	enginectl.Dir = t.TempDir()
	t.Cleanup(func() { enginectl.Dir = oldDir })
	cfg := config.Config{Server: config.ServerConfig{BindAddr: "127.0.0.1:0", Transport: config.TCP, Token: "reload-test", Preset: "turbo", SkipOptz: true}}
	write := func() {
		t.Helper()
		var b bytes.Buffer
		if err := toml.NewEncoder(&b).Encode(cfg); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	old := enableThroughputCapacity
	t.Cleanup(func() { enableThroughputCapacity = old })
	applied := make(chan struct{}, 1)
	enableThroughputCapacity = func([]int) (string, error) { applied <- struct{}{}; return "", nil }
	ctx, cancel := context.WithTimeout(context.Background(), 6*configPollInterval)
	done := make(chan struct{})
	go func() { defer close(done); Run(path, ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	// Let the watcher take the first fingerprint. Initial startup skips all
	// sysctl writes; only the native capacity call is replaced in this test.
	time.Sleep(configPollInterval)
	select {
	case <-applied:
		t.Fatal("Turbo enabled host capacity")
	default:
	}
	cfg.Server.Preset, cfg.Server.SkipOptz = "throughput", false
	write()
	select {
	case <-applied:
	case <-ctx.Done():
		t.Fatal("Throughput reload did not apply host capacity")
	}
}
