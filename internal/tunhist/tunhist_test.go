package tunhist

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Samples in the same hour share a bucket; a new hour opens a new one, and
// the bucket keeps the newest cumulative value with an honest up-count.
func TestHourlyBucketing(t *testing.T) {
	f := File{Tunnels: map[string]*History{}}
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

	f.Record("t1", true, base)
	f.Record("t1", false, base.Add(5*time.Minute))
	f.Record("t1", true, base.Add(time.Hour))

	h := f.Tunnels["t1"]
	if len(h.Recent) != 3 {
		t.Fatalf("want 3 recent samples, got %d", len(h.Recent))
	}
	if len(h.Hourly) != 2 {
		t.Fatalf("want 2 hourly buckets, got %d", len(h.Hourly))
	}
	first := h.Hourly[0]
	if first.N != 2 || first.UpN != 1 {
		t.Fatalf("first bucket wrong: %+v", first)
	}
}

// Both rings must stay bounded no matter how long the sampler runs.
func TestRingsAreTrimmed(t *testing.T) {
	f := File{Tunnels: map[string]*History{}}
	base := time.Now().Truncate(time.Hour)
	for i := 0; i < keepRecent+50; i++ {
		f.Record("t1", true, base.Add(time.Duration(i)*5*time.Minute))
	}
	h := f.Tunnels["t1"]
	if len(h.Recent) != keepRecent {
		t.Fatalf("recent not trimmed: %d", len(h.Recent))
	}
	if len(h.Hourly) > keepHourly {
		t.Fatalf("hourly not trimmed: %d", len(h.Hourly))
	}
}

// A deleted tunnel must not stay in the file forever.
func TestPrune(t *testing.T) {
	f := File{Tunnels: map[string]*History{}}
	f.Record("gone", true, time.Now())
	f.Record("kept", true, time.Now())
	f.prune(map[string]bool{"kept": true})
	if _, ok := f.Tunnels["gone"]; ok {
		t.Fatal("deleted tunnel still present")
	}
	if _, ok := f.Tunnels["kept"]; !ok {
		t.Fatal("live tunnel was pruned")
	}
}

// The round trip through disk must preserve the store.
func TestSaveLoad(t *testing.T) {
	previous := Dir
	Dir = t.TempDir()
	t.Cleanup(func() { Dir = previous })
	f := File{Tunnels: map[string]*History{}, Updated: time.Now()}
	f.Record("t1", true, time.Now())
	if err := save(f); err != nil {
		t.Fatal(err)
	}
	got := Load()
	if h := got.Tunnels["t1"]; h == nil || len(h.Recent) != 1 || !h.Recent[0].Up {
		t.Fatalf("round trip lost data: %+v", got.Tunnels)
	}
}

func TestSamplingPreservesHistoryItCannotRead(t *testing.T) {
	previous := Dir
	Dir = t.TempDir()
	t.Cleanup(func() { Dir = previous })
	for _, damaged := range []string{`{"tunnels":`, `null`, `{}`, `{"tunnels":null}`} {
		if err := os.WriteFile(path(), []byte(damaged), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := load(); err == nil {
			t.Errorf("accepted incomplete history %s", damaged)
		}
		samplePass()
		if body, err := os.ReadFile(path()); err != nil || string(body) != damaged {
			t.Fatalf("unreadable history changed to %q, error %v", body, err)
		}
	}
}

func TestSamplingPrunesDeletedTunnelsAndKeepsUnreadableConfigs(t *testing.T) {
	previous := Dir
	Dir = t.TempDir()
	t.Cleanup(func() { Dir = previous })
	f := File{Tunnels: map[string]*History{}}
	now := time.Now()
	f.Record("broken", true, now)
	f.Record("deleted", true, now)
	if err := save(f); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Dir, "broken.toml"), []byte("[broken"), 0600); err != nil {
		t.Fatal(err)
	}
	samplePass()
	got, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if h := got.Tunnels["broken"]; h == nil || len(h.Recent) != 1 || !h.Recent[0].Up {
		t.Fatalf("history for the existing tunnel changed: %+v", h)
	}
	if _, ok := got.Tunnels["deleted"]; ok {
		t.Fatal("history for the deleted tunnel was retained")
	}
}
