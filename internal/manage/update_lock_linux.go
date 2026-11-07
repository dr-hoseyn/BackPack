package manage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/backpack/backpack/internal/app"
	"golang.org/x/sys/unix"
)

func lockUpdate() (func(), error) {
	return lockUpdateDir(app.InstallDir)
}

func lockUpdateDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("prepare update lock: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, ".update.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open update lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another update or rollback is already in progress: %w", err)
		}
		return nil, fmt.Errorf("lock update: %w", err)
	}
	// Keep the sidecar inode across updates. Closing releases the lock, even
	// when the process exits unexpectedly.
	return func() { _ = file.Close() }, nil
}
