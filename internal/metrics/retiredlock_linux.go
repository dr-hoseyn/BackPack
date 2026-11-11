package metrics

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockRetiredFile(file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
