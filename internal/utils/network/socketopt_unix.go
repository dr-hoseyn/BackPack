//go:build !windows

package network

import "syscall"

func setSocketInt(fd uintptr, level, option, value int) error {
	return syscall.SetsockoptInt(int(fd), level, option, value)
}

func setTCPMSS(fd uintptr, value int) error {
	return setSocketInt(fd, syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, value)
}
