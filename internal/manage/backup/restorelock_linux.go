package backup

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// The lock file sits beside the live tree so it keeps the same inode during
// the swap. Closing it releases the lock even if the process exits mid-restore.
func lockRestore(configDir string) (func(), error) {
	file, err := os.OpenFile(configDir+".restore.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open restore lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another configuration restore is in progress: %w", err)
		}
		return nil, fmt.Errorf("lock configuration restore: %w", err)
	}
	return func() { _ = file.Close() }, nil
}
