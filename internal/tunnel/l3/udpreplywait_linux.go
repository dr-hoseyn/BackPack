//go:build linux

package l3

import (
	"errors"
	"net"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// RawConn.Read uses Go's network poller, including the socket's deadline and
// Close wakeup. A nonblocking readiness check leaves every datagram intact,
// including zero-length packets. No payload storage is held while idle.
type udpReplyWaiter struct {
	raw     syscall.RawConn
	fds     [1]unix.PollFd
	readErr error
	ready   func(uintptr) bool
}

func newUDPReplyWaiter(socket *net.UDPConn) (*udpReplyWaiter, error) {
	raw, err := socket.SyscallConn()
	if err != nil {
		return nil, err
	}
	w := &udpReplyWaiter{raw: raw}
	// Reuse the callback and its poll descriptor across reads as well as
	// payload buffers. RawConn's callback must not allocate per datagram.
	w.ready = w.check
	return w, nil
}

func (w *udpReplyWaiter) check(fd uintptr) bool {
	w.fds[0] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
	for {
		var ready int
		ready, w.readErr = unix.Poll(w.fds[:], 0)
		if errors.Is(w.readErr, unix.EINTR) {
			continue
		}
		return w.readErr != nil || ready != 0
	}
}

func (w *udpReplyWaiter) wait() error {
	err := w.raw.Read(w.ready)
	if err != nil {
		return err
	}
	if w.readErr != nil {
		return os.NewSyscallError("poll", w.readErr)
	}
	return nil
}
