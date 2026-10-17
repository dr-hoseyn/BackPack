//go:build !linux

package manage

import "io"

func ctTerminalSize(io.Writer) (int, int) { return 0, 0 }
