package core

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/backpack/backpack/internal/metrics"
)

func TestRenameRejectsExistingDestinationTrafficState(t *testing.T) {
	for _, tc := range []struct {
		kind string
		path func(string, string) string
	}{
		{"traffic counters", metrics.Path},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "old.toml"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkRenameFiles(dir, "old", "new"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tc.path(dir, "new"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkRenameFiles(dir, "old", "new"); err == nil || !strings.Contains(err.Error(), tc.kind) {
				t.Fatalf("destination check returned %v, want error about %s", err, tc.kind)
			}
		})
	}
}

func TestRenamePreservesRunningAndEnabledStatesIndependently(t *testing.T) {
	for _, tc := range []struct {
		name            string
		active, enabled bool
		want            []string
	}{
		{name: "stopped and disabled"},
		{name: "stopped and enabled", enabled: true, want: []string{"enable renamed.service"}},
		{name: "running and disabled", active: true, want: []string{"start renamed.service"}},
		{name: "running and enabled", active: true, enabled: true, want: []string{"enable renamed.service", "start renamed.service"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			if err := restoreRenameService("renamed.service", tc.active, tc.enabled, func(args ...string) (string, error) {
				calls = append(calls, strings.Join(args, " "))
				return "", nil
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(calls, tc.want) {
				t.Fatalf("systemctl calls = %v, want %v", calls, tc.want)
			}
		})
	}
}

func TestRenameRollsBackConfigAndTrafficWhenUnitPreparationFails(t *testing.T) {
	dir := t.TempDir()
	files := []func(string, string) string{
		func(dir, name string) string { return filepath.Join(dir, name+".toml") },
		metrics.Path,
	}
	for _, path := range files {
		if err := os.WriteFile(path(dir, "old"), []byte("preserved"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	failure := errors.New("systemd reload failed")
	err := moveRenameFiles(dir, "old", "new", func() error {
		for _, path := range files {
			if _, err := os.Stat(path(dir, "new")); err != nil {
				t.Errorf("file was not ready when preparing the unit: %v", err)
			}
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("rename error = %v, want %v", err, failure)
	}
	for _, path := range files {
		if data, err := os.ReadFile(path(dir, "old")); err != nil || string(data) != "preserved" {
			t.Errorf("old file = %q, error %v", data, err)
		}
		if _, err := os.Stat(path(dir, "new")); !os.IsNotExist(err) {
			t.Errorf("new file still exists: %v", err)
		}
	}
}
