//go:build !linux

package network

import "net"

func tcpMuxSocketPressure(net.Conn) (int, bool) { return 0, false }

func newTCPMuxPressureReader(net.Conn) func() (int, bool) { return nil }

func sampleMuxTCP(net.Conn) MuxTCPSnapshot { return MuxTCPSnapshot{} }

func limitTCPMuxBacklog(net.Conn) func() { return nil }
