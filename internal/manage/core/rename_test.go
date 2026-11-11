package core

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/quota"
)

func TestRenameRejectsExistingDestinationTrafficState(t *testing.T) {
	for _, tc := range []struct {
		kind string
		path func(string, string) string
	}{
		{"traffic counters", metrics.Path},
		{"traffic quota", quota.Path},
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
