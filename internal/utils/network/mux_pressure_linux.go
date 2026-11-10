//go:build linux

package network

import (
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Bound bytes waiting to enter TCP, while leaving the congestion window and
// socket-buffer autotuning available to fill a high-bandwidth, high-RTT path.
// https://kernel.org/doc/html/latest/networking/ip-sysctl.html#tcp-notsent-lowat
// The returned tuner is called only by the session's single heartbeat writer.
func limitTCPMuxBacklog(conn net.Conn) func() {
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return nil
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return nil
	}
	current := 512 << 10
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		optionErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, current)
	}); err != nil || optionErr != nil {
		return nil
	}
	return func() {
		_ = raw.Control(func(fd uintptr) {
			info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
			if err != nil {
				return
			}
			next := tcpMuxQueueReserve(info)
			// Ignore small pacing fluctuations rather than changing the
			// socket's wake-up threshold on every heartbeat.
			if next >= current+current/4 || next <= current-current/4 {
				if unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, next) == nil {
					current = next
				}
			}
		})
	}
}

func tcpMuxQueueReserve(info *unix.TCPInfo) int {
	const floor, ceiling, step = 512 << 10, 2 << 20, 128 << 10
	rate, rtt := info.Pacing_rate, uint64(info.Min_rtt)
	if rate == 0 || rate == ^uint64(0) || rtt == 0 || info.Min_rtt == ^uint32(0) {
		return floor
	}
	// Keep one eighth of the paced bandwidth-delay product ready to send.
	// Check the cap before multiplying, including for large kernel counters.
	if rate >= uint64(ceiling)*8_000_000/rtt {
		return ceiling
	}
	reserve := int(rate * rtt / 8_000_000)
	return max(floor, (reserve+step-1)/step*step)
}

func tcpMuxSocketPressure(conn net.Conn) (int, bool) {
	socket, ok := conn.(syscall.Conn)
	if !ok {
		return 0, false
	}
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, false
	}
	var pressure int
	var queued bool
	_ = raw.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err == nil {
			pressure = tcpMuxQueuePressure(info)
			queued = info.Notsent_bytes != 0 || tcpMuxRetransmissionStalled(info)
		}
	})
	return pressure, queued
}

func tcpMuxQueuePressure(info *unix.TCPInfo) int {
	// An unused TCP window costs more on a high-RTT path. Bound the scale
	// so a badly queued carrier can still yield even on a very slow route.
	step := time.Duration(min(max(int(info.Min_rtt/2), 5_000), 50_000)) * time.Microsecond
	return min(int(tcpMuxQueueDelay(info)/step), 64)
}

func tcpMuxQueueDelay(info *unix.TCPInfo) time.Duration {
	// Small stream-open frames can all be in flight on a blackholed path:
	// Notsent_bytes is zero, but new users still wait for retransmission.
	if tcpMuxRetransmissionStalled(info) {
		return time.Second
	}
	// RTT inflation without a send backlog can be a delayed ACK from the
	// previous small request. It does not queue the next user's bytes.
	if info.Notsent_bytes == 0 {
		return 0
	}
	// Persist probes identify a known closed receiver window without
	// treating absent Snd_wnd fields on older kernels as a closed window.
	if info.Snd_wnd == 0 && info.Probes > 0 {
		return time.Second
	}
	var micros uint64
	if info.Min_rtt > 0 && info.Rtt > info.Min_rtt {
		micros = uint64(info.Rtt - info.Min_rtt)
	}
	rate := info.Delivery_rate
	if info.Rtt > 0 {
		if rate == 0 {
			rate = uint64(info.Snd_cwnd) * uint64(info.Snd_mss) * 1_000_000 / uint64(info.Rtt)
		}
		// A recent high delivery rate cannot bypass today's smaller
		// receive window. Do not replace a genuinely slow rate by cwnd/RTT.
		if info.Snd_wnd > 0 {
			windowRate := uint64(info.Snd_wnd) * 1_000_000 / uint64(info.Rtt)
			if rate == 0 || windowRate < rate {
				rate = windowRate
			}
		}
	}
	if rate > 0 {
		micros += uint64(info.Notsent_bytes) * 1_000_000 / rate
	}
	if micros > 1_000_000 {
		micros = 1_000_000
	}
	return time.Duration(micros) * time.Microsecond
}

func tcpMuxRetransmissionStalled(info *unix.TCPInfo) bool {
	return info.Unacked > 0 && info.Retransmits > 0
}
