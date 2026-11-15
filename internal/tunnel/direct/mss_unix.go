//go:build !windows

package direct

import (
	"net"
	"syscall"
)

// clampMSS caps what this end puts in one TCP segment.
//
// It is applied to a connected socket rather than in the dialler, because both
// ends need it and only one of them dials: the origin's connections arrive from
// Accept, where there is no dial to configure. Linux takes TCP_MAXSEG on an
// established socket and clamps what is sent from then on, which is exactly the
// half of the problem this end owns — the other end clamps its own.
//
// Best effort by design. A kernel that will not take it leaves the tunnel
// working exactly as it did before, which is the right outcome for a knob that
// is off unless somebody set it.
func clampMSS(tcp *net.TCPConn, mss int) {
	if mss <= 0 {
		return
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_MAXSEG, mss)
	})
}
