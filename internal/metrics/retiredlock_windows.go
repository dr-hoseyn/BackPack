package metrics

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockRetiredFile(file *os.File) error {
	var offset windows.Overlapped
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &offset)
}
