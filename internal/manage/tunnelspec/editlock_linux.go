package tunnelspec

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// The CLI and panel are separate processes. Lock a stable sidecar inode, not
// the config inode that atomic writes replace. The file must not be unlinked.
func lockConfigFile(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another settings change is in progress; wait for it to finish and review again: %w", err)
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}
