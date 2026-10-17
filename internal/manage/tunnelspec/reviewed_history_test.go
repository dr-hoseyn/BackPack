package tunnelspec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/app"
)

func TestEditHistoryPreviewAndReviewedRestore(t *testing.T) {
	withTempConfigDir(t)
	file, err := os.CreateTemp(app.ConfigDir, "reviewed-history-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	path := file.Name()
	file.Close()
	t.Cleanup(func() {
		os.Remove(path)
		os.Remove(path + ".edit.lock")
	})
	name := strings.TrimSuffix(filepath.Base(path), ".toml")
	current := "[l3]\nmtu=1400\n"
	saved := "[l3]\nmtu=1300\n"
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	assertUnchanged := func(body string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || string(got) != body {
			t.Fatalf("current config changed: %q, %v", got, err)
		}
		if got := ConfigHistory(name); len(got) != 1 || got[0].Prev != saved {
			t.Fatalf("history changed: %+v", got)
		}
	}
	write(current)
	RecordChange(name, []byte(saved), "original settings")
	entries := ConfigHistory(name)
	if len(entries) != 1 {
		t.Fatalf("saved history entries: %d, want 1", len(entries))
	}
	entry := entries[0]
	preview, err := PreviewConfigFrom(name, entry.At)
	if err != nil || preview.Revision != ConfigRevision([]byte(current)) || len(preview.Changes) != 1 {
		t.Fatalf("history preview: %+v, %v", preview, err)
	}
	if diff := preview.Changes[0]; diff.Field != "l3.mtu" || diff.Before != "1400" || diff.After != "1300" {
		t.Fatalf("wrong restore diff: %+v", diff)
	}
	assertUnchanged(current)

	// A writer that commits after review must not be overwritten by a restore.
	newer := "[l3]\nmtu=1500\n"
	write(newer)
	if err := RestoreConfigFrom(name, entry.At, preview.Revision); !errors.Is(err, ErrSettingsChanged) {
		t.Fatalf("stale restore was not rejected: %v", err)
	}
	assertUnchanged(newer)

	// Restoring an already-current snapshot succeeds without contacting systemd
	// or adding another history entry. Both legacy and reviewed writers share it.
	write(saved)
	if err := RestoreConfigFrom(name, entry.At, ConfigRevision([]byte(saved))); err != nil {
		t.Fatalf("no-op reviewed restore: %v", err)
	}
	if err := ApplyBody(name, saved, "no-op legacy edit"); err != nil {
		t.Fatalf("no-op legacy edit: %v", err)
	}
	assertUnchanged(saved)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewBody(name, saved); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preview did not report the missing current config: %v", err)
	}
}

func TestEditHistoryAndPreviewRejectInvalidTargets(t *testing.T) {
	withTempConfigDir(t)
	at := time.Now()
	if _, err := PreviewBody("../escape", "[l3]\nmtu=1300\n"); err == nil {
		t.Error("preview accepted a path traversal")
	}
	if _, err := PreviewConfigFrom("../escape", at); err == nil {
		t.Error("history preview accepted a path traversal")
	}
	if err := RestoreConfigFrom("../escape", at); err == nil {
		t.Error("history restore accepted a path traversal")
	}
	if err := ApplyBodyReviewed("../escape", "[l3]\nmtu=1300\n", "", ""); err == nil {
		t.Error("reviewed edit accepted a path traversal")
	}
	if _, err := PreviewConfigFrom("missing-history", at); err == nil {
		t.Error("preview accepted a missing history entry")
	}
}
