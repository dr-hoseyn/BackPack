package network

import (
	"context"
	"fmt"
	"net"
	"syscall"
	"time"
)

// TcpDialer opens a connection straight to remoteAddress, by whatever route the
// kernel would choose.
//
// This is the dialer for anything that must not be redirected — above all the
// dial to the local backend, which never leaves the machine, and which a proxy
// or an interface binding meant for the uplink would simply break. It has no
// way to take either, which is what keeps that traffic off them by construction
// rather than by remembering. Tunnel connections use TcpDialerVia.
func TcpDialer(ctx context.Context, remoteAddress string, timeout time.Duration, keepAlive time.Duration, nodelay bool, retry int, SO_RCVBUF int, SO_SNDBUF, mss int) (*net.TCPConn, error) {
	conn, err := TcpDialerVia(ctx, nil, remoteAddress, timeout, keepAlive, nodelay, retry, SO_RCVBUF, SO_SNDBUF, mss)
	if err != nil {
		return nil, err
	}
	// A direct dial never has proxy bytes to preserve and always stays TCP.
	if tcp, ok := conn.(*net.TCPConn); ok {
		return tcp, nil
	}
	conn.Close()
	return nil, fmt.Errorf("direct TCP dial returned %T", conn)
}

// TcpDialerVia opens a connection to remoteAddress the way out describes: from
// a chosen source address or interface, marked for a routing rule, through a
// proxy, or any combination — and straight to it when out is nil.
//
// The socket is dialled here whatever out says, so it carries the tunnel's
// buffer sizes, congestion control, keepalive and MSS whether it ends up
// talking to the server or to a proxy standing in front of it; only where it
// goes and what handshake follows differ. Anything out asks for that cannot be
// applied fails the attempt, and the retry and backoff below cover it exactly
// as a refused dial would be.
// HTTP proxies may return a connection preserving prefetched tunnel bytes;
// direct, SOCKS and header-only CONNECT dials retain their raw TCP connection.
func TcpDialerVia(ctx context.Context, out *Outbound, remoteAddress string, timeout time.Duration, keepAlive time.Duration, nodelay bool, retry int, SO_RCVBUF int, SO_SNDBUF, mss int) (net.Conn, error) {
	var tcpConn *net.TCPConn
	var err error

	// With a proxy the socket is opened to the proxy, and the address the
	// caller asked for becomes what the handshake asks the proxy to reach.
	dialAddress := out.dialAddress(remoteAddress)
	proxy := out.proxy()

	retries := max(retry, 1)   // Always make at least one attempt
	backoff := 1 * time.Second // Initial backoff duration

	for i := 0; i < retries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// DNS, socket setup and proxy negotiation share one attempt budget.
		attemptCtx := ctx
		cancelAttempt := func() {}
		if timeout > 0 {
			attemptCtx, cancelAttempt = context.WithTimeout(ctx, timeout)
		}
		tcpConn, err = attemptTcpDialer(attemptCtx, dialAddress, out, timeout, keepAlive, nodelay, SO_RCVBUF, SO_SNDBUF, mss)
		var result net.Conn = tcpConn
		if err == nil && proxy != nil {
			proxyConn := tcpConn
			stop := context.AfterFunc(attemptCtx, func() { proxyConn.Close() })
			remaining := timeout
			if deadline, ok := attemptCtx.Deadline(); ok {
				remaining = time.Until(deadline)
			}
			if err = attemptCtx.Err(); err == nil {
				result, err = connectThrough(tcpConn, proxy, remoteAddress, remaining)
			}
			stop()
			if err != nil {
				tcpConn.Close()
				tcpConn = nil
				err = fmt.Errorf("via %s: %w", proxy, err)
			}
		}
		if err == nil && attemptCtx.Err() != nil {
			tcpConn.Close()
			err = attemptCtx.Err()
		}
		cancelAttempt()
		if err == nil {
			// Connection successful
			return result, nil
		}

		// If this is the last retry, return the error
		if i == retries-1 {
			break
		}

		// A cancelled generation must release its retry worker immediately,
		// including time spent waiting between refused TCP or proxy dials.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2 // Exponential backoff (double the wait time after each failure)
	}

	return nil, err
}

// func attemptTcpDialer(ctx context.Context, address string, timeout time.Duration, keepAlive time.Duration, nodelay bool, SO_RCVBUF int, SO_SNDBUF int) (*net.TCPConn, error) {
func attemptTcpDialer(
	ctx context.Context,
	remoteAddress string,
	out *Outbound,
	timeout time.Duration,
	keepAlive time.Duration,
	nodelay bool,
	SO_RCVBUF int,
	SO_SNDBUF int,
	mss int,
) (*net.TCPConn, error) {

	// The address is deliberately not resolved here.
	//
	// It used to be, through net.ResolveTCPAddr, which returns exactly one
	// address — the first IPv4 one — and that address was then the only one
	// ever dialled. A server name with several A records is the ordinary way to
	// publish more than one route to the same machine, and it is what an
	// operator reaches for when one IP starts being filtered: add another,
	// leave the name alone. Resolving to a single address threw all of that
	// away. The first record was tried forever, and if it was the blocked one
	// the tunnel simply never connected, while every other record sat there
	// working.
	//
	// Handing the name to the dialer instead gets Go's own behaviour: it
	// resolves the lot and tries them in turn until one answers, splitting the
	// timeout across the attempts, and races IPv4 against IPv6 (RFC 6555) so a
	// host with an AAAA record that does not actually route no longer costs the
	// whole connection. This is what rathole and gost both do, by dialling the
	// name rather than an address.
	localTCPAddr, err := out.localTCPAddrContext(ctx)
	if err != nil {
		return nil, err
	}

	// Options
	dialer := &net.Dialer{
		Control: func(network, address string, s syscall.RawConn) error {
			// No address-reuse options on an outgoing socket: it binds an
			// ephemeral port it has no reason to share, and asking used to
			// abort the dial outright wherever the kernel refused. See
			// reuse_port.go.

			// The interface and fwmark are applied first: they were asked
			// for by name, and a
			// connection that leaves by the wrong link is worse than one that
			// does not leave at all. See outbound.go.
			if err := out.outboundControl(network, address, s); err != nil {
				return err
			}

			return configureTCPSocket(s, SO_RCVBUF, SO_SNDBUF, mss)

		},
		Timeout:         timeout, // Set the connection timeout
		KeepAliveConfig: net.KeepAliveConfig{Enable: true, Interval: keepAlive, Count: 9, Idle: 0},
		LocalAddr:       localTCPAddr,
	}

	// Dial by name, so every address it resolves to gets a turn.
	dialer.SetMultipathTCP(out != nil && out.MPTCP)
	conn, err := dialer.DialContext(ctx, "tcp", remoteAddress)
	if err != nil {
		return nil, err
	}

	// Type assert the net.Conn to *net.TCPConn
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("failed to convert net.Conn to *net.TCPConn")
	}

	if !nodelay {
		err = tcpConn.SetNoDelay(false)
		if err != nil {
			tcpConn.Close()
			return nil, fmt.Errorf("failed to set TCP_NODELAY to %v: %w", nodelay, err)
		}
	}

	return tcpConn, nil
}
