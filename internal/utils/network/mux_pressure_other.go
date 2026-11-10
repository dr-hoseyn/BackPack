//go:build !linux

package network

import (
	"net"
	"time"
)

func tcpMuxDelay(net.Conn) time.Duration { return 0 }
