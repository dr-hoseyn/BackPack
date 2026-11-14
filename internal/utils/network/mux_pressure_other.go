//go:build !linux

package network

import "net"

func tcpMuxSocketPressure(net.Conn) (int, bool) { return 0, false }

func tcpMuxSocketPressureKnown(net.Conn) (int, bool, bool) { return 0, false, false }

func limitTCPMuxBacklog(net.Conn) func() { return nil }
