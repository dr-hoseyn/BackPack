package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSnapshotCopyReplacesFileWithSavedPermissions(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "saved"), filepath.Join(dir, "live")
	if err := os.WriteFile(src, []byte("saved token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("changed token"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "saved token" {
		t.Fatalf("restored content = %q (%v)", got, err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("restored token permissions = %o", info.Mode().Perm())
	}
	occupied := filepath.Join(dir, "directory")
	if err := os.Mkdir(occupied, 0700); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, occupied, 0600); err == nil {
		t.Fatal("replaced a directory with a file")
	}
	if staged, err := filepath.Glob(filepath.Join(dir, ".snapshot-copy-*")); err != nil || len(staged) != 0 {
		t.Fatalf("copy leaked staging files: %v (%v)", staged, err)
	}
}

func TestListSnapshotsOrdersSameSecondAndSkipsIncompleteCopies(t *testing.T) {
	root := t.TempDir()
	write := func(name, created string) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if created == "" {
			return
		}
		data, err := json.Marshal(SnapshotMeta{Stamp: "20261106-010203", Created: created})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("pre-update-20261106-010203", "2026-11-06T01:02:03Z")
	write("pre-update-20261106-010203-900", "2026-11-06T01:02:03.1Z")
	write("pre-update-20261106-010203-100", "2026-11-06T01:02:03.2Z")
	write("pre-update-20261106-010204-200", "")
	got := listSnapshots(root)
	want := []string{"pre-update-20261106-010203-100", "pre-update-20261106-010203-900", "pre-update-20261106-010203"}
	if len(got) != len(want) {
		t.Fatalf("unexpected restore points: %+v", got)
	}
	for i, snapshot := range got {
		if filepath.Base(snapshot.Dir) != want[i] {
			t.Fatalf("restore point %d is %s, want %s", i, snapshot.Dir, want[i])
		}
	}
}
