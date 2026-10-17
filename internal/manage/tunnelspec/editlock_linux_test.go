package tunnelspec

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
	defer unlock()
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
