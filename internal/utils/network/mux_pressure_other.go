//go:build !linux

package network

import "net"

func tcpMuxSocketPressure(net.Conn) (int, bool) { return 0, false }

func limitTCPMuxBacklog(net.Conn) func() { return nil }
