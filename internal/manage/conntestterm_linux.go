package manage

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func ctTerminalSize(out io.Writer) (int, int) {
	if f, ok := out.(*os.File); ok {
		if size, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil {
			return int(size.Col), int(size.Row)
		}
	}
	return 0, 0
}
