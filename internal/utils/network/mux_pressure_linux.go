//go:build linux

package network

import (
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func tcpMuxDelay(conn net.Conn) time.Duration {
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return 0
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0
	}
	var delay time.Duration
	_ = raw.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err == nil {
			delay = tcpMuxQueueDelay(info)
		}
	})
	return delay
}

func tcpMuxQueueDelay(info *unix.TCPInfo) time.Duration {
	var micros uint64
	if info.Min_rtt > 0 && info.Rtt > info.Min_rtt {
		micros = uint64(info.Rtt - info.Min_rtt)
	}
	rate := info.Delivery_rate
	if rate == 0 && info.Rtt > 0 {
		rate = uint64(info.Snd_cwnd) * uint64(info.Snd_mss) * 1_000_000 / uint64(info.Rtt)
	}
	if rate > 0 {
		micros += uint64(info.Notsent_bytes) * 1_000_000 / rate
	}
	if micros > 1_000_000 {
		micros = 1_000_000
	}
	return time.Duration(micros) * time.Microsecond
}
