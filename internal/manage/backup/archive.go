package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	backupRetention = 10
	backupPrefix    = "backpack-backup-"
	backupSuffix    = ".tar.gz"
	partialPrefix   = "." + backupPrefix
	partialSuffix   = ".partial"
	partialAge      = time.Hour
)

type backupArchive struct {
	path     string
	modified time.Time
}

// listBackupArchives orders published regular files by modification time.
// Existing archives with second-resolution names are included.
func listBackupArchives(dir string) ([]backupArchive, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var archives []backupArchive
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, backupPrefix) || !strings.HasSuffix(name, backupSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		archives = append(archives, backupArchive{filepath.Join(dir, name), info.ModTime()})
	}
	sort.Slice(archives, func(i, j int) bool {
		if archives[i].modified.Equal(archives[j].modified) {
			return archives[i].path > archives[j].path
		}
		return archives[i].modified.After(archives[j].modified)
	})
	return archives, nil
}

func pruneBackups(dir string) {
	sweepPartials(dir)
	archives, _ := listBackupArchives(dir)
	if len(archives) <= backupRetention {
		return
	}
	for _, old := range archives[backupRetention:] {
		_ = os.Remove(old.path)
	}
}

// An interrupted process may leave a partial archive in the backup directory.
func sweepPartials(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, partialPrefix) || !strings.HasSuffix(name, partialSuffix) {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && time.Since(info.ModTime()) >= partialAge {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// publishBackup links the synced archive into place. Linking publishes the
// entire file and fails if another writer already owns the destination.
func publishBackup(dir string, write func(io.Writer) error) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, partialPrefix+"*"+partialSuffix)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	// CreateTemp opens the archive with mode 0600.
	if err := write(tmp); err != nil {
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(tmp.Name()), partialPrefix), partialSuffix)
	stamp := time.Now().Format("20060102-150405.000000000")
	path := filepath.Join(dir, backupPrefix+stamp+"-"+id+backupSuffix)
	if err := os.Link(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("publish backup: %w", err)
	}

	syncDir(dir)
	pruneBackups(dir)
	return path, nil
}

// syncDir flushes a directory entry after publication or a restore.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
