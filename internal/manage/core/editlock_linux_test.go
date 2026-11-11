package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
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

func TestConcurrentRestoresCannotStageOverEachOther(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backpack")
	release, err := lockConfigTree(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockConfigTree(dir, true); err == nil {
		second()
		release()
		t.Fatal("a second restore acquired the lock")
	} else if !errors.Is(err, unix.EWOULDBLOCK) {
		release()
		t.Fatalf("unexpected lock error: %v", err)
	}
	release()

	next, err := lockConfigTree(dir, true)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	next()
}

func TestConfigRestoreExcludesTunnelChangesInBothDirections(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backpack")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		release, err := lockTunnelConfig(dir, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	if unlock, err := lockConfigTree(dir, true); !errors.Is(err, unix.EWOULDBLOCK) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("restore while two tunnels are locked = %v", err)
	}

	otherDir := filepath.Join(t.TempDir(), "backpack")
	restore, err := lockConfigTree(otherDir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if unlock, err := lockTunnelConfig(otherDir, "first"); !errors.Is(err, unix.EWOULDBLOCK) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("tunnel change while restore is locked = %v", err)
	}
}
