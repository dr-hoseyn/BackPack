package manage

import (
	"strings"
	"testing"
)

func TestUpdateLockRejectsAnotherOpenAndReleasesOnClose(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockUpdateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	second, err := lockUpdateDir(dir)
	if err == nil {
		second()
		t.Fatal("two independent update handles acquired the lock")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Fatal(err)
	}
	unlock()
	second, err = lockUpdateDir(dir)
	if err != nil {
		t.Fatalf("update lock was not released: %v", err)
	}
	second()
}
