package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/backpack/backpack/internal/metrics"
)

// Reload must finish the old health writer before starting the next generation.
// Neither generation persists byte consumption.
func TestReloadKeepsHealthSnapshotsWithoutPersistingConsumption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nl.toml")
	ctx := context.Background()

	stop := startMetrics(ctx, path, "tcp", "server")
	metrics.AddBytes(1000, 400)
	stop() // the generation ends: its last write must be in when this returns
	snap, err := metrics.Read(dir, "nl")
	if err != nil {
		t.Fatal(err)
	}
	if snap.BytesIn != 0 || snap.BytesOut != 0 || snap.Transport != "tcp" {
		t.Fatalf("health snapshot contains consumption: %+v", snap)
	}

	stop = startMetrics(ctx, path, "tcp", "server")
	metrics.AddBytes(10, 5)
	stop()
	snap, err = metrics.Read(dir, "nl")
	if err != nil {
		t.Fatal(err)
	}
	if snap.BytesIn != 0 || snap.BytesOut != 0 || snap.Transport != "tcp" {
		t.Fatalf("reloaded health snapshot contains consumption: %+v", snap)
	}
}
