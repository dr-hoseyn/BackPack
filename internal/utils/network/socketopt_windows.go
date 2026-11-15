package network

import (
	"errors"
	"syscall"
)

func setSocketInt(fd uintptr, level, option, value int) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), level, option, value)
}

func setTCPMSS(fd uintptr, value int) error {
	return errors.New("TCP_MAXSEG is not available on Windows")
}
