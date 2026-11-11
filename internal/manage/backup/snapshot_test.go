package backup

import (
	"encoding/json"
	"errors"
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

func TestSnapshotRestoreStagesAllFilesBeforeChangingLiveConfigs(t *testing.T) {
	parent := t.TempDir()
	source, live := filepath.Join(parent, "snapshot"), filepath.Join(parent, "live")
	for _, dir := range []string{source, live} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeAt(t, live, "t1.toml", "current config", 0600)
	writeAt(t, source, "t1.toml", "saved config", 0600)
	// A saved directory cannot replace an existing regular file.
	writeAt(t, live, "z-conflict", "keep this file", 0600)
	if err := os.Mkdir(filepath.Join(source, "z-conflict"), 0700); err != nil {
		t.Fatal(err)
	}
	if stage, err := stageSnapshotConfig(source, live); err == nil {
		os.RemoveAll(stage)
		t.Fatal("staging accepted a file/directory conflict")
	}
	assertStaged(t, live, "t1.toml", "current config")
	assertStaged(t, live, "z-conflict", "keep this file")
	stages, err := filepath.Glob(filepath.Join(parent, ".backpack-snapshot-restore-*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("failed snapshot staging left %v, error %v", stages, err)
	}
}

func TestSnapshotConfigFailureRestoresTheInstalledBinary(t *testing.T) {
	dir := t.TempDir()
	source, target := filepath.Join(dir, "saved"), filepath.Join(dir, "installed")
	if err := os.WriteFile(source, []byte("snapshot binary"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.WriteFile(target, []byte("installed binary"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		failure := errors.New("config swap failed")
		err := installSnapshotBinary(source, target, func() error {
			if got, err := os.ReadFile(target); err != nil || string(got) != "snapshot binary" {
				t.Errorf("snapshot binary was not installed: %q, error %v", got, err)
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatalf("restore error = %v, want %v", err, failure)
		}
		body, err := os.ReadFile(target)
		if existing {
			if err != nil || string(body) != "installed binary" {
				t.Fatalf("original binary was lost: %q, error %v", body, err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("new binary was retained after rollback: %q, error %v", body, err)
		}
	}
}
