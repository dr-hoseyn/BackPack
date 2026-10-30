package network

import (
	"context"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The pinned Chrome ClientHello reduces a Go-specific handshake signature;
// it does not emulate a complete browser or make traffic unidentifiable.
// Authenticated direct connections verify the session-bound server proof;
// bearer-token/legacy connections must verify the certificate before HTTP.
func uTLSClientConn(ctx context.Context, raw net.Conn, serverName string, timeout time.Duration, http11Only bool, verified ...*utls.Config) (*utls.UConn, error) {
	cfg := &utls.Config{ServerName: serverName, InsecureSkipVerify: true}
	if len(verified) > 0 && verified[0] != nil {
		cfg = verified[0].Clone()
	}

	uconn := utls.UClient(raw, cfg, utls.HelloChrome_133)

	if http11Only {
		if err := pinClientALPNToHTTP11(uconn); err != nil {
			raw.Close()
			return nil, err
		}
	}

	if timeout > 0 {
		_ = raw.SetDeadline(time.Now().Add(timeout))
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})

	// The Chrome fingerprint carries the renegotiation_info extension, and
	// applying it flips on renegotiation support — which, as a side effect,
	// disables the RFC 5705 keying-material exporter the credential binding
	// relies on. The extension is already on the wire by now, so the fingerprint
	// is unaffected; turning renegotiation back off re-enables the exporter
	// without changing a single byte that was sent. (We never renegotiate — TLS
	// 1.3 cannot — so nothing else depends on this.)
	cfg.Renegotiation = utls.RenegotiateNever

	return uconn, nil
}

// sniFromAddr picks the server name to present in the handshake. For a hostname
// that is the hostname; for a bare IP it is empty, because a browser sends no
// SNI when it dials an address literal and the fingerprint should match.
func sniFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if net.ParseIP(host) != nil {
		return ""
	}
	return host
}

// pinClientALPNToHTTP11 selects the protocol this implementation can carry.
// Chromium has an HTTP/1.1-only path for fresh WebSocket sockets; a full HTTPS
// ClientHello is not the only browser behavior. See Chromium's
// net/http/http_stream_factory_job.cc and its WebSocket socket-pool path.
// This changes the ALPN contents and therefore the ClientHello. Classification
// or blocking cannot be inferred from that change alone. The caller chooses
// it explicitly, or after an authenticated endpoint selected unsupported h2.
func pinClientALPNToHTTP11(uconn *utls.UConn) error {
	// The preset's extensions do not exist until the hello is built, and the
	// ALPN list inside it is hardcoded by the fingerprint rather than read from
	// the config — so setting NextProtos alone changes nothing here.
	if err := uconn.BuildHandshakeState(); err != nil {
		return fmt.Errorf("wss: building the client hello: %w", err)
	}
	for _, ext := range uconn.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = []string{"http/1.1"}
		}
	}
	// The handshake re-marshals the hello from these extensions, so the change
	// is on the wire without anything else being touched.
	return nil
}
