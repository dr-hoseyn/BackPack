package schedule

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Crontab updates are a read-modify-write across two commands. Keep the lock
// outside the config tree so a restore cannot replace its inode.
func lockCrontab() (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(home, ".backpack-cron.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open crontab update lock: %w", err)
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock crontab update: %w", err)
	}
	return func() { _ = file.Close() }, nil
}
