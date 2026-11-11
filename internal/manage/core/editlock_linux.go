package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Management commands can run in separate processes. Lock a stable sidecar inode, not
// the config inode that atomic writes replace. The file must not be unlinked.
func lockConfigFile(path string) (func(), error) {
	return lockConfigPath(path, unix.LOCK_EX, "another settings change is in progress")
}

// This file lives beside the config tree, so swapping the directory does not
// replace the locked inode. Ordinary changes may proceed in parallel.
func lockConfigTree(dir string, exclusive bool) (func(), error) {
	mode := unix.LOCK_SH
	busy := "a configuration backup or restore is in progress"
	if exclusive {
		mode = unix.LOCK_EX
		busy = "another configuration backup, restore or tunnel change is in progress"
	}
	return lockConfigPath(filepath.Clean(dir)+".restore.lock", mode, busy)
}

func lockConfigPath(path string, mode int, busy string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s; wait for it to finish and review again: %w", busy, err)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = file.Close() }, nil
}
