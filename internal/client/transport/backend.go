package transport

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/utils"
	"github.com/backpack/backpack/internal/utils/handlers"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/xtaci/smux"
)

// The last hop: from the tunnel to the service a user actually asked for, on
// this machine or one it can reach.
//
// Every transport ends here, and each had its own copy of it — pick a healthy
// backend, choose the socket buffers, dial, report a failure the way
// localdial.go words it, relay — with the multiplexed ones also carrying a copy
// of the loop that takes streams off a session. They differed only in their
// socket options, which are backendOpts.

// backendOpts is how a transport's users reach the local service.
type backendOpts struct {
	// owner narrows a mux backend's lifetime to its physical session. The
	// control channel and sibling sessions may remain healthy when one fails.
	owner       context.Context
	dialTimeout time.Duration
	keepAlive   time.Duration
	// rcvBuf and sndBuf are the socket buffers for a backend on another
	// machine; a loopback backend always gets 32 KB. Zero leaves the kernel's.
	rcvBuf int
	sndBuf int
	mss    int
	// sniffer records per-port usage for the panel.
	sniffer    bool
	relayFrame int // optional WSSMux destination frame size
}

func (o backendOpts) context(generation context.Context) context.Context {
	if o.owner != nil {
		return o.owner
	}
	return generation
}

// loopbackBuffer is the socket buffer for a backend on this machine, where a
// large buffer only adds latency.
const loopbackBuffer = 32 * 1024

// isLoopback reports whether addr is on this machine: any loopback address, v4
// or v6, or localhost.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// dialBackend tries the configured backends before any user data is sent.
func (l *lifecycle) dialBackend(addr string, o backendOpts) net.Conn {
	ctx := o.context(l.state.Ctx())
	if ctx.Err() != nil {
		return nil
	}
	candidates := backends.candidates(addr)
	dialCtx := ctx
	if len(candidates) > 1 && o.dialTimeout > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, o.dialTimeout)
		defer cancel()
	}
	var lastAddr string
	var lastErr error
	for i, candidate := range candidates {
		if dialCtx.Err() != nil {
			break
		}
		timeout := o.dialTimeout
		if deadline, ok := dialCtx.Deadline(); ok && len(candidates) > 1 {
			// Give the remaining backends a chance even if one silently drops
			// packets. An early refusal leaves its unused time to the rest.
			timeout = time.Until(deadline) / time.Duration(len(candidates)-i)
			if timeout <= 0 {
				break
			}
		}
		rcv, snd := o.rcvBuf, o.sndBuf
		if isLoopback(candidate) {
			rcv, snd = loopbackBuffer, loopbackBuffer
		}
		conn, err := network.TcpDialer(dialCtx, candidate, timeout, o.keepAlive, true, 1, rcv, snd, o.mss)
		if err != nil {
			lastAddr, lastErr = candidate, err
			continue
		}
		ReportLocalDialOK()
		l.logger.Debugf("connected to local address %s successfully", candidate)
		return conn
	}
	if lastErr != nil && ctx.Err() == nil {
		localDial.Report(l.logger, lastAddr, lastErr)
	}
	return nil
}

// relay dials the backend for one user already resolved to addr and port, and
// carries the user's connection to it until either side closes.
func (l *lifecycle) relay(user net.Conn, addr string, port int, o backendOpts) {
	backend := l.dialBackend(addr, o)
	if backend == nil {
		user.Close()
		return
	}
	handlers.TCPConnectionHandler(o.context(l.state.Ctx()), false, metrics.CountedConn(user), backend, l.logger, l.state.Usage(), port, o.sniffer, handlers.RelayReadFrames{To: o.relayFrame})
}

// relayStream serves one user arriving on a stream that names its own target:
// a UDP flow is handed to the UDP forwarder, anything else is resolved and
// relayed.
func (l *lifecycle) relayStream(stream net.Conn, target string, o backendOpts) {
	if dialForwardedUDP(o.context(l.state.Ctx()), o.dialTimeout, stream, target, l.logger, l.state.Usage(), o.sniffer) {
		return
	}
	port, addr, err := network.ResolveRemoteAddr(target)
	if err != nil {
		l.logger.Infof("failed to resolve remote port: %v", err)
		stream.Close()
		return
	}
	l.relay(stream, addr, int(port), o)
}

// serveSession takes the streams off one mux session until it ends, each one a
// user: the target it names is read in the stream's own goroutine, so a stream
// that is slow to name one never holds up the streams behind it.
func (l *lifecycle) serveSession(session *smux.Session, from net.Addr, o backendOpts) {
	ctx, cancel := context.WithCancel(l.state.Ctx())
	o.owner = ctx
	// The session is the generation's: when the generation ends the session
	// goes with it, and AcceptStream below returns. Waiting for the peer to
	// notice instead is waiting for ever over KCP, where nothing tells the
	// socket the far side has gone.
	stop := context.AfterFunc(ctx, func() { session.Close() })
	defer func() {
		stop()
		// Ending one data session must also cancel its pending DNS/dials and
		// idle backend flows. Waiting for the whole generation stranded them
		// across pool repairs while the control channel stayed connected.
		cancel()
		// However the loop ended, the session ends with it — including when
		// the generation was already over before the first AcceptStream.
		session.Close()
	}()
	for ctx.Err() == nil {
		stream, err := session.AcceptStream()
		if err != nil {
			l.logger.Trace("session is closed: ", err)
			return
		}
		l.state.Go(func() {
			_ = stream.SetReadDeadline(time.Now().Add(controlAckTimeout))
			target, err := utils.ReceiveBinaryString(stream)
			if err != nil {
				l.logger.Errorf("unable to get port from stream connection %s: %v", from, err)
				stream.Close()
				return
			}
			_ = stream.SetReadDeadline(time.Time{})
			l.relayStream(stream, target, o)
		})
	}
}
