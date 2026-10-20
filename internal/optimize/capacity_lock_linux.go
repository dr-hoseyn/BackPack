//go:build linux

package optimize

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Lock a stable sidecar inode: the profile and backup are replaced atomically
// and cannot serve as lock files. Never unlink this file after releasing it.
func lockKernelPolicy() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(capacityLockFile), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(capacityLockFile, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another kernel policy update is in progress: %w", err)
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}
