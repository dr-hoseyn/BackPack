//go:build !windows
// +build !windows

package network

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// ListenWithBuffers creates a TCP listener with specified SO_RCVBUF and SO_SNDBUF sizes.
// It returns a net.Listener and an error if any.
func ListenWithBuffers(network, address string, rcvBufSize, sndBufSize, mss int, keepAlivePeriod time.Duration, dis_nodelay bool, multipath ...bool) (net.Listener, error) {
	// Options
	listenerCfg := &net.ListenConfig{
		Control: func(network, address string, s syscall.RawConn) error {
			// SO_REUSEADDR only, best effort — never SO_REUSEPORT, which would
			// let a leftover process share this port instead of colliding with
			// it. See reuse_port.go.
			err := ListenReuseControl(network, address, s)
			if err != nil {
				return err
			}

			if err := configureTCPSocket(s, rcvBufSize, sndBufSize, mss); err != nil {
				return err
			}
			return controlSocket(s, func(fd uintptr) error {
				value := 1
				if dis_nodelay {
					value = 0
				}
				if err := setSocketInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, value); err != nil {
					return fmt.Errorf("failed to set TCP_NODELAY: %w", err)
				}
				return nil
			})

		},
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Interval: keepAlivePeriod, Count: 9, Idle: 0},
	}

	listenerCfg.SetMultipathTCP(len(multipath) != 0 && multipath[0])
	// Bind with a timeout. Go falls back to TCP if MPTCP is unavailable.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	listener, err := listenerCfg.Listen(ctx, network, address)
	if err != nil {
		return nil, err
	}

	return listener, err
}
