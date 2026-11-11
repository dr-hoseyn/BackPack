package backup

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConcurrentRestoresCannotStageOverEachOther(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backpack")
	release, err := lockRestore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockRestore(dir); err == nil {
		second()
		release()
		t.Fatal("a second restore acquired the lock")
	} else if !strings.Contains(err.Error(), "another configuration restore") {
		release()
		t.Fatalf("unexpected lock error: %v", err)
	}
	release()

	next, err := lockRestore(dir)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	next()
}
