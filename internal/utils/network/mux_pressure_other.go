//go:build !linux

package network

import (
	"errors"
	"net"
)

func tcpMuxSocketPressure(net.Conn) (int, bool) { return 0, false }

func SetWSMuxSendLowWatermark(net.Conn, int) error {
	return errors.New("TCP_NOTSENT_LOWAT is only available on Linux")
}

func sampleMuxTCP(net.Conn) MuxTCPSnapshot { return MuxTCPSnapshot{} }

func limitTCPMuxBacklog(net.Conn) func() { return nil }
