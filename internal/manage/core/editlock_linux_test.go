package core

import (
	"path/filepath"
	"testing"
)

func TestEditLockSerializesIndependentFileHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel.edit.lock")
	unlock, err := lockConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockConfigFile(path); err == nil {
		second()
		t.Fatal("a second writer acquired the same tunnel lock")
	}
	unlock()
	next, err := lockConfigFile(path)
	if err != nil {
		t.Fatalf("released lock could not be reacquired: %v", err)
	}
	next()
}

func TestRenameLockReleasesSourceWhenDestinationIsBusy(t *testing.T) {
	dir := t.TempDir()
	release, err := lockConfigFile(filepath.Join(dir, "destination.toml.edit.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if unlock, err := lockRenameConfigs(dir, "source", "destination"); err == nil {
		unlock()
		t.Fatal("rename acquired an already locked destination")
	}
	unlock, err := lockConfigFile(filepath.Join(dir, "source.toml.edit.lock"))
	if err != nil {
		t.Fatalf("failed rename left the source locked: %v", err)
	}
	unlock()
}
