package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
