package network

import "net"

// MuxTCPSnapshot contains local send-side counters for one physical carrier.
// Available distinguishes a missing kernel sample from an empty send queue.
type MuxTCPSnapshot struct {
	Available    bool
	NotSentBytes uint32
	TotalRetrans uint32
	RTTUS        uint32
}

// SampleMuxTCP unwraps the transport only for socket observation.
func SampleMuxTCP(conn net.Conn) MuxTCPSnapshot {
	return sampleMuxTCP(tcpMuxPressureConn(conn))
}
