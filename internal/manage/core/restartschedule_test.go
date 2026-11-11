package core

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Both servers are told the same hours and minute and must fire at the same
// instant wherever they are, so the calendar is written in UTC; and what the
// setup link hands on is read back from the timer, not remembered elsewhere.
func TestTheRestartScheduleIsUTCAndReadsBack(t *testing.T) {
	for _, tc := range []struct {
		hours, minute int
		cal           string
	}{
		{6, 17, "*-*-* 00/6:17:00 UTC"},
		{1, 0, "*-*-* 00/1:00:00 UTC"},
		{24, 5, "*-*-* 00:05:00 UTC"},
		{48, 59, "*-*-01/2 00:59:00 UTC"},
	} {
		cal := restartCalendar(tc.hours, tc.minute)
		if cal != tc.cal {
			t.Errorf("%dh:%d → %q, want %q", tc.hours, tc.minute, cal, tc.cal)
		}
		m := restartCalendarRe.FindStringSubmatch("OnCalendar=" + cal)
		if m == nil {
			t.Errorf("%q does not read back", cal)
		}
	}
	if !strings.HasPrefix(scheduledRestartUnit("k1", "timer"), "backpack-restart-") {
		t.Error("the timer must not be named like a tunnel's own unit")
	}
	if EffectiveRestartHours(36) != 24 || EffectiveRestartHours(0) != 0 || EffectiveRestartHours(7) != 7 {
		t.Error("EffectiveRestartHours")
	}
}

func TestRestartScheduleRemovalPreservesFilesWhenSystemdCannotStopTimer(t *testing.T) {
	dir := t.TempDir()
	const name = "example"
	for _, kind := range []string{"timer", "service"} {
		if err := os.WriteFile(filepath.Join(dir, scheduledRestartUnit(name, kind)), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	failure := errors.New("failed to disable")
	err := removeScheduledRestartAt(dir, name, func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "systemd refused", failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("removal returned %v, want %v", err, failure)
	}
	if want := []string{"disable --now backpack-restart-example.timer"}; !slices.Equal(calls, want) {
		t.Fatalf("systemctl calls = %v, want %v", calls, want)
	}
	for _, kind := range []string{"timer", "service"} {
		if _, err := os.Stat(filepath.Join(dir, scheduledRestartUnit(name, kind))); err != nil {
			t.Errorf("%s unit disappeared after disable failed: %v", kind, err)
		}
	}
}

func TestRestartScheduleRemovalCanRetryDaemonReload(t *testing.T) {
	dir := t.TempDir()
	const name = "example"
	path := filepath.Join(dir, scheduledRestartUnit(name, "service"))
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	failure := errors.New("reload failed")
	systemctl := func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		if len(calls) == 1 {
			return "systemd refused", failure
		}
		return "", nil
	}
	if err := removeScheduledRestartAt(dir, name, systemctl); !errors.Is(err, failure) {
		t.Fatalf("first removal = %v, want %v", err, failure)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("orphan service file still exists: %v", err)
	}
	if err := removeScheduledRestartAt(dir, name, systemctl); err != nil {
		t.Fatal(err)
	}
	if want := []string{"daemon-reload", "daemon-reload"}; !slices.Equal(calls, want) {
		t.Fatalf("systemctl calls = %v, want %v", calls, want)
	}
}
