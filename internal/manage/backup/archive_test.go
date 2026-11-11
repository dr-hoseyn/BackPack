package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConcurrentBackupsKeepTheirOwnContents(t *testing.T) {
	dir := t.TempDir()
	type result struct {
		path, body string
		err        error
	}
	const writers = backupRetention
	results := make(chan result, writers)
	ready := make(chan struct{}, writers)
	start := make(chan struct{})
	for i := range writers {
		go func() {
			body := fmt.Sprintf("archive %d", i)
			path, err := publishBackup(dir, func(w io.Writer) error {
				ready <- struct{}{}
				<-start
				_, err := io.WriteString(w, body)
				return err
			})
			results <- result{path, body, err}
		}()
	}
	for range writers {
		select {
		case <-ready:
		case r := <-results:
			close(start)
			t.Fatalf("backup stopped before writing: %v", r.err)
		}
	}
	close(start)
	seen := make(map[string]bool, writers)
	for range writers {
		r := <-results
		if r.err != nil {
			t.Error(r.err)
			continue
		}
		if seen[r.path] {
			t.Errorf("multiple writers received %s", r.path)
		}
		seen[r.path] = true
		got, err := os.ReadFile(r.path)
		if err != nil || string(got) != r.body {
			t.Errorf("reading %s: got %q, error %v, want %q", r.path, got, err, r.body)
		}
	}
}

func TestBackupRetentionUsesTheNewestPublishedArchives(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := range backupRetention + 2 {
		// All files have a legacy timestamp. Their suffixes differ in length.
		path := filepath.Join(dir, fmt.Sprintf("backpack-backup-20260101-000000-%d.tar.gz", i))
		if err := os.WriteFile(path, []byte("archive"), 0600); err != nil {
			t.Fatal(err)
		}
		modified := now.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "another-app.tar.gz")
	if err := os.WriteFile(unrelated, nil, 0600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "backpack-backup-directory.tar.gz")
	if err := os.Mkdir(fake, 0700); err != nil {
		t.Fatal(err)
	}
	pruneBackups(dir)

	archives, err := listBackupArchives(dir)
	if err != nil || len(archives) != backupRetention {
		t.Fatalf("got %d archives, error %v, want %d archives", len(archives), err, backupRetention)
	}
	for i, archive := range archives {
		want := fmt.Sprintf("backpack-backup-20260101-000000-%d.tar.gz", backupRetention+1-i)
		if got := filepath.Base(archive.path); got != want {
			t.Errorf("archive %d = %s, want %s", i, got, want)
		}
	}
	for _, path := range []string{unrelated, fake} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("pruning affected %s: %v", path, err)
		}
	}
}
