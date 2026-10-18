// Package httpcarrier provides restricted TrustTunnel CONNECT streams and
// authenticated WebTransport streams/datagrams. Every listener has one fixed
// loopback destination; client-controlled requests cannot select another target.
package httpcarrier

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

// Fragment larger tunnel packets without introducing reliable delivery. A lost
// fragment drops that packet; IPv6's 1280-byte minimum still fits this carrier.
const MaxDatagram = 65535

func Validate(protocol, mode, username, password, path string) error {
	if protocol != "trusttunnel" && protocol != "webtransport" {
		return errors.New("unknown HTTP carrier")
	}
	if mode != "" && mode != "auto" && mode != "h2" && mode != "h3" {
		return errors.New("HTTP carrier mode must be auto, h2 or h3")
	}
	if protocol == "webtransport" && mode != "" && mode != "h3" {
		return errors.New("WebTransport requires HTTP/3")
	}
	if len(username) < 1 || len(username) > 128 || strings.ContainsAny(username, ":\x00\r\n") || len(password) < 16 || len(password) > 1024 || strings.ContainsAny(password, "\x00\r\n") {
		return errors.New("HTTP carrier requires a username and a separate 16..1024 byte password")
	}
	if protocol == "webtransport" && (len(path) < 2 || len(path) > 512 || path[0] != '/' || strings.ContainsAny(path, "?#\\ \t\r\n%")) {
		return errors.New("WebTransport requires a private absolute path without escapes, query or fragment")
	}
	if protocol == "trusttunnel" && path != "" {
		return errors.New("TrustTunnel CONNECT does not use a URL path")
	}
	return nil
}

func FixedTarget(target string) error {
	h, p, err := net.SplitHostPort(target)
	ip := net.ParseIP(h)
	n, pe := strconv.Atoi(p)
	if err != nil || pe != nil || ip == nil || !ip.IsLoopback() || n < 1 || n > 65535 {
		return errors.New("HTTP carrier target must be a literal loopback IP:port")
	}
	return nil
}

func verifiedTLS(t *tls.Config) (*tls.Config, error) {
	if t == nil || t.InsecureSkipVerify {
		return nil, errors.New("HTTP carriers require verified TLS")
	}
	t = t.Clone()
	t.MinVersion = tls.VersionTLS13
	return t, nil
}

func authorization(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}
func authorized(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
func quicConfig() *quic.Config {
	return &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, MaxIncomingStreams: 128, MaxIncomingUniStreams: 128, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second}
}

// Relay preserves directional EOF and waits for both copies before releasing
// their sockets. Cancellation also interrupts completely idle transfers.
func Relay(ctx context.Context, local net.Conn, remote io.ReadWriteCloser) {
	stop := context.AfterFunc(ctx, func() { local.Close(); remote.Close() })
	defer stop()
	defer local.Close()
	defer remote.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	copyTo := func(dst io.Writer, src io.Reader) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if c, ok := dst.(interface{ CloseWrite() error }); err == nil && ok {
			if c.CloseWrite() == nil {
				return
			}
		}
		local.Close()
		remote.Close()
	}
	go copyTo(remote, local)
	go copyTo(local, remote)
	wg.Wait()
}

type webStream struct {
	*webtransport.Stream
	finished atomic.Bool
}

func (s *webStream) CloseWrite() error {
	err := s.Stream.Close()
	if err == nil {
		s.finished.Store(true)
	}
	return err
}
func (s *webStream) Close() error {
	s.CancelRead(0)
	if !s.finished.Load() {
		s.CancelWrite(0)
	}
	return nil
}

// Bound the complete WebTransport negotiation, including a peer that accepts
// QUIC and then withholds its CONNECT response. Detach that deadline only after
// the session is established, so it cannot later terminate a healthy session.
// Callers serialize access to tr during negotiation.
func dialWebTransport(ctx context.Context, tr *webtransport.Transport, endpoint string, headers http.Header) (*http.Response, *webtransport.Session, error) {
	stop := func() bool { return true }
	tr.DialAddr = func(ctx context.Context, addr string, t *tls.Config, q *quic.Config) (*quic.Conn, error) {
		conn, err := quic.DialAddr(ctx, addr, t, q)
		if err == nil {
			stop = context.AfterFunc(ctx, func() { conn.CloseWithError(0, "setup deadline") })
		}
		return conn, err
	}
	r, s, err := tr.Dial(ctx, endpoint, headers)
	stop()
	return r, s, err
}
