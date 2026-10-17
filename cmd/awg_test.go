package cmd

import (
	"context"
	"github.com/BurntSushi/toml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/amneziawg"
	"github.com/backpack/backpack/internal/metrics"
)

func TestAWGFinalMetricsWaitForTheFinalDeviceSample(t *testing.T) {
	old := runManagedAWG
	defer func() { runManagedAWG = old }()
	ready := make(chan struct{})
	runManagedAWG = func(ctx context.Context, _ amneziawg.Config, _ io.Writer, report func(amneziawg.RuntimeStats)) error {
		report(amneziawg.RuntimeStats{})
		close(ready)
		<-ctx.Done()
		// Model a transfer completed since the last one-second IPC sample.
		report(amneziawg.RuntimeStats{BytesIn: 123456, BytesOut: 654321, Endpoint: "203.0.113.1:25999", LastHandshake: time.Now()})
		return nil
	}
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	c := config.Config{AWG: config.AWGConfig{PeerAddress: "10.20.0.2/32"}}
	go func() { defer close(done); runAWGTunnel(&c, ctx, filepath.Join(dir, "awg-final.toml")) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not stop")
	}
	s, err := metrics.Read(dir, "awg-final")
	if err != nil {
		t.Fatal(err)
	}
	if s.BytesIn != 123456 || s.BytesOut != 654321 {
		t.Fatalf("final device counters were lost: %d/%d", s.BytesIn, s.BytesOut)
	}
}

func TestAWGEngineChecksPrivateFilePermissionsAndExclusiveConfiguration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux managed engine validation")
	}
	a, _, err := amneziawg.NewPair("203.0.113.1:25999", "10.254.248.1/32", "10.254.248.2/32")
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	if err := toml.NewEncoder(&body).Encode(struct {
		AWG config.AWGConfig `toml:"awg"`
	}{config.AWGConfig(a)}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "private-awg.toml")
	if err := os.WriteFile(path, []byte(body.String()), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("accepted world-readable private identities")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkEngine(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Server.BindAddr = "127.0.0.1:3080"
	if checkEngine(cfg) == nil {
		t.Fatal("mixed AWG and reverse engines were accepted")
	}
}
