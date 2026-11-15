package network

import (
	"fmt"
	"syscall"
)

// controlSocket preserves both descriptor-access and socket-option failures.
func controlSocket(s syscall.RawConn, configure func(uintptr) error) error {
	var optionErr error
	if err := s.Control(func(fd uintptr) { optionErr = configure(fd) }); err != nil {
		return err
	}
	return optionErr
}

func configureTCPSocket(s syscall.RawConn, rcvBuf, sndBuf, mss int) error {
	return controlSocket(s, func(fd uintptr) error {
		if PinTCPBuffers() && rcvBuf > 0 {
			if err := setSocketInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBuf); err != nil {
				return fmt.Errorf("failed to set SO_RCVBUF: %w", err)
			}
		}
		if PinTCPBuffers() && sndBuf > 0 {
			if err := setSocketInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, sndBuf); err != nil {
				return fmt.Errorf("failed to set SO_SNDBUF: %w", err)
			}
		}
		// Congestion control is a best-effort tuning hint.
		setCongestion(fd, TCPCongestion)
		if mss > 0 {
			if err := setTCPMSS(fd, mss); err != nil {
				return fmt.Errorf("failed to set MSS: %w", err)
			}
		}
		return nil
	})
}
