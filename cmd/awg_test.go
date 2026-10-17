package cmd

import (
	"context"
	"io"
	"path/filepath"
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
