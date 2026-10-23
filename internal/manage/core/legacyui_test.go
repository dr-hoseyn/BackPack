package core

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRetiredPanelIsStoppedBeforeItsUnitIsRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backpack-webui.service")
	if err := os.WriteFile(path, []byte("old unit"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	run := func(args ...string) (string, error) {
		calls = append(calls, args)
		if args[0] == "disable" {
			if _, err := os.Stat(path); err != nil {
				t.Fatal("unit removed before stopping")
			}
		}
		return "", nil
	}
	if err := retireLegacyPanel(dir, run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("retired unit remains")
	}
	if !reflect.DeepEqual(calls, [][]string{{"disable", "--now", "backpack-webui.service"}, {"daemon-reload"}}) {
		t.Fatal(calls)
	}
	if err := retireLegacyPanel(dir, run); err != nil || len(calls) != 2 {
		t.Fatal("cleanup is not idempotent")
	}
}

func TestRetiredPanelCleanupKeepsTheUnitWhenStopFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backpack-webui.service")
	if err := os.WriteFile(path, []byte("old unit"), 0600); err != nil {
		t.Fatal(err)
	}
	err := retireLegacyPanel(dir, func(...string) (string, error) { return "", errors.New("systemctl failed") })
	if err == nil {
		t.Fatal("stop failure was hidden")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed a unit that could not be stopped")
	}
}
