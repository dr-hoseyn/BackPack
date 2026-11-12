//go:build !linux

package l3

import "net"

// Other platforms retain the normal blocking Read. They still share bounded
// cached buffers, but the idle-buffer optimization requires Linux's poller.
type udpReplyWaiter struct{}

func newUDPReplyWaiter(_ *net.UDPConn) (*udpReplyWaiter, error) {
	return &udpReplyWaiter{}, nil
}

func (*udpReplyWaiter) wait() error { return nil }
