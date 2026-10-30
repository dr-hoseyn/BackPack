package network

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/gorilla/websocket"
	utls "github.com/refraction-networking/utls"
)

func WebSocketDialer(ctx context.Context, out *Outbound, addr string, edgeIP string, path string, timeout time.Duration, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, simpleAuth bool, retry int, SO_RCVBUF int, SO_SNDBUF int, mss int, options ...WSDialOption) (*websocket.Conn, error) {
	var tunnelWSConn *websocket.Conn
	var err error

	retries := max(retry, 1)   // Always make at least one attempt
	backoff := 1 * time.Second // Initial backoff duration

	for i := 0; i < retries; i++ {
		// Attempt to dial the WebSocket
		tunnelWSConn, err = attemptDialWebSocket(ctx, out, addr, edgeIP, path, timeout, keepalive, nodelay, token, mode, simpleAuth, SO_RCVBUF, SO_SNDBUF, mss, options...)
		if err == nil {
			// If successful, return the connection
			if ctx.Err() != nil {
				tunnelWSConn.Close()
				return nil, ctx.Err()
			}
			return tunnelWSConn, nil
		}
		// Retrying the same unauthenticated peer cannot repair a missing or
		// invalid proof. Let the owning reconnect/failover loop decide next.
		if errors.Is(err, errWSSServerUnproven) {
			return nil, err
		}

		// If this is the last retry, return the error
		if i == retries-1 {
			break
		}

		// Log the retry attempt and wait before retrying
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

func attemptDialWebSocket(ctx context.Context, out *Outbound, addr string, edgeIP string, path string, timeout time.Duration, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, simpleAuth bool, SO_RCVBUF int, SO_SNDBUF int, mss int, options ...WSDialOption) (result *websocket.Conn, resultErr error) {
	// Socket setup, both possible TLS handshakes and HTTP Upgrade share one
	// budget. gorilla applies a deadline to Upgrade I/O but does not interrupt
	// that I/O when a context without a deadline is cancelled.
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var owned net.Conn
	var stopClose func() bool
	var closeFinished chan struct{}
	stopWatching := func() {
		if stopClose != nil {
			if !stopClose() {
				<-closeFinished
			}
			stopClose = nil
		}
	}
	own := func(conn net.Conn) {
		stopWatching()
		owned = conn
		finished := make(chan struct{})
		closeFinished = finished
		stopClose = context.AfterFunc(ctx, func() {
			conn.Close()
			close(finished)
		})
	}
	defer func() {
		// Detach and join the cancellation callback before handing ownership
		// to the transport or cancelling this setup-only context.
		stopWatching()
		if err := ctx.Err(); err != nil {
			resultErr = err
		} else if resultErr != nil {
			if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				resultErr = context.DeadlineExceeded
			}
		}
		if resultErr != nil {
			if owned != nil {
				owned.Close()
			}
			result = nil
		}
	}()
	// Keep HTTP identity consistent with the pinned uTLS Chrome 133 preset.
	// Random UA families and custom user-id headers do not emulate a browser.
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36")
	settings := wsDialSettings{tls: &WSSClientOptions{}}
	for _, option := range options {
		if option != nil {
			option.applyWS(&settings)
		}
	}
	var muxOptions *WSMuxOptions
	if (mode == config.WSMUX || mode == config.WSSMUX) && path != "/channel" {
		muxOptions = settings.mux
		if muxOptions != nil {
			muxOptions.offer(headers)
		}
	}

	var wsURL string
	dialer := websocket.Dialer{}
	// ekm is the TLS session's keying material on a bound wss dial, for
	// checking the server's answer; nil otherwise.
	var ekm []byte
	var learnedALPN string
	rawMux := path != "/channel" && (mode == config.WSMUX || mode == config.WSSMUX)

	// Handle edgeIP assignment
	if edgeIP != "" {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid address format, failed to parse: %w", err)
		}

		// JoinHostPort rather than string concatenation: an IPv6 edge address
		// needs brackets, and gluing "::1" to a port produces something that
		// will not resolve.
		edgeIP = net.JoinHostPort(edgeIP, port)
	} else {
		edgeIP = addr
	}

	// path generation
	if path != "/channel" {
		path = path + "/" + hex.EncodeToString(suffix[:])
	}

	switch mode {
	case config.WS, config.WSMUX:
		wsURL = fmt.Sprintf("ws://%s%s", addr, path)

		// Plain ws has no session to bind to, so the token is the credential.
		headers.Set("Authorization", fmt.Sprintf("Bearer %v", token))

		dialer = websocket.Dialer{
			EnableCompression: false,
			// The relay hands the websocket up to 64 KiB at a time; gorilla's
			// default 4 KiB buffers sent each of those as sixteen writes. See
			// the server transport's wsWriteBufferSize.
			ReadBufferSize:  wsReadBufferSize,
			WriteBufferSize: wsWriteBufferSize,
			WriteBufferPool: wsWriteBuffers,
			NetDialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
				conn, err := TcpDialerVia(dialCtx, out, edgeIP, timeout, keepalive, nodelay, 1, SO_RCVBUF, SO_SNDBUF, mss)
				if err != nil {
					return nil, err
				}
				own(conn)
				if rawMux {
					return muxUpgradeStream(conn), nil
				}
				return conn, nil
			},
		}
	case config.WSS, config.WSSMUX:
		wsURL = fmt.Sprintf("wss://%s%s", addr, path)

		// The SNI names the real server even when the TCP connection goes to a
		// CDN edge, and it is what the browser fingerprint would carry.
		sni := sniFromAddr(addr)
		if settings.tls.ServerName != "" {
			sni = settings.tls.ServerName
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			headers.Set("Host", net.JoinHostPort(sni, port))
		}
		verify := simpleAuth || settings.tls.AllowLegacyServer || settings.tls.CAFile != ""
		if verify && sni == "" {
			sni, _, _ = net.SplitHostPort(addr)
		}
		roots, err := settings.tls.rootCAs()
		if err != nil {
			return nil, err
		}

		// The TLS handshake is done up front, before the websocket upgrade, so
		// its keying material can be bound into the credential. gorilla is then
		// handed the finished connection and only writes the HTTP upgrade over
		// it — it does no TLS of its own, and TLSClientConfig is not consulted.
		tlsAttempt := func(http11Only bool) (*utls.UConn, error) {
			rawConn, err := TcpDialerVia(ctx, out, edgeIP, timeout, keepalive, nodelay, 1, SO_RCVBUF, SO_SNDBUF, mss)
			if err != nil {
				return nil, err
			}
			own(rawConn)
			return uTLSClientConn(ctx, rawConn, sni, timeout, http11Only, &utls.Config{ServerName: sni, RootCAs: roots, InsecureSkipVerify: !verify})
		}

		// Cache only ALPN capability, never credentials or trust. An endpoint
		// change/failure expires the decision; direct peers retain the full offer.
		cacheKey := addr + "\x00" + edgeIP + "\x00" + sni
		http11 := wssALPN.http11(cacheKey, time.Now())
		uconn, err := tlsAttempt(http11)
		if err != nil {
			wssALPN.forget(cacheKey)
			return nil, err
		}
		if uconn.ConnectionState().NegotiatedProtocol == "h2" {
			uconn.Close()
			uconn, err = tlsAttempt(true)
			if err != nil {
				wssALPN.forget(cacheKey)
				return nil, err
			}
			learnedALPN = cacheKey
		}

		// With a trusted TLS-terminating proxy in front of the server (NGINX,
		// typically), the proxy's TLS session is not the client's, so a bound
		// proof can never match what the server computes. simpleAuth sends the
		// raw token instead — the same credential the plain ws transport sends —
		// which the operator has chosen to trust that proxy with. Without a
		// proxy this is a downgrade, so it is off by default.
		if simpleAuth {
			headers.Set("Authorization", "Bearer "+token)
		} else {
			cs := uconn.ConnectionState()
			if ekm, err = cs.ExportKeyingMaterial(WSSBindingLabel, nil, wssBindingLength); err != nil {
				uconn.Close()
				return nil, fmt.Errorf("wss: could not bind the credential to the TLS session: %w", err)
			}
			proof, err := wssClientBinding(uconn, token)
			if err != nil {
				uconn.Close()
				return nil, fmt.Errorf("wss: could not bind the credential to the TLS session: %w", err)
			}
			headers.Set("Authorization", "Bearer "+proof)
		}

		dialer = websocket.Dialer{
			EnableCompression: false,
			// The relay hands the websocket up to 64 KiB at a time; gorilla's
			// default 4 KiB buffers sent each of those as sixteen writes. See
			// the server transport's wsWriteBufferSize.
			ReadBufferSize:  wsReadBufferSize,
			WriteBufferSize: wsWriteBufferSize,
			WriteBufferPool: wsWriteBuffers,
			NetDialTLSContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				if rawMux {
					return muxUpgradeStream(uconn), nil
				}
				return uconn, nil
			},
		}
	}

	if rawMux {
		dialer.Subprotocols = []string{WSMuxBinaryProtocol}
	}
	// Dial to the WebSocket server
	tunnelWSConn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return nil, err
	}
	if ekm != nil {
		if err := checkWSSServer(ekm, token, addr, resp.Header.Get(WSSServerProofHeader), settings.tls.AllowLegacyServer); err != nil {
			tunnelWSConn.Close()
			return nil, err
		}
	}
	if muxOptions != nil {
		if err := muxOptions.accept(resp.Header); err != nil {
			tunnelWSConn.Close()
			return nil, err
		}
	}
	if learnedALPN != "" {
		wssALPN.remember(learnedALPN, time.Now())
	}
	return tunnelWSConn, nil
}

// The websocket buffers on the client side; see the server transport's
// wsWriteBufferSize for why these sizes and why the write buffers are pooled.
const (
	wsReadBufferSize  = 16 * 1024
	wsWriteBufferSize = 64*1024 + 8 // one maximum smux payload plus its header
)

var wsWriteBuffers = &sync.Pool{}
