package httpcarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"golang.org/x/net/http2"
)

type Client struct {
	ctx                                  context.Context
	cancel                               context.CancelFunc
	protocol, mode, endpoint, path, auth string
	h2                                   *http2.Transport
	h3                                   *http3.Transport
	wt                                   *webtransport.Transport
	mu                                   sync.Mutex
	session                              *webtransport.Session
	wtStarted                            bool
	once                                 sync.Once
}

func NewClient(parent context.Context, protocol, mode, endpoint, user, password, path string, cfg *tls.Config) (*Client, error) {
	if err := Validate(protocol, mode, user, password, path); err != nil {
		return nil, err
	}
	if h, p, err := net.SplitHostPort(endpoint); err != nil || h == "" || p == "" {
		return nil, errors.New("HTTP carrier endpoint must be host:port")
	}
	t, err := verifiedTLS(cfg)
	if err != nil {
		return nil, err
	}
	if t.ServerName == "" {
		return nil, errors.New("HTTP carrier requires an explicit TLS identity")
	}
	ctx, cancel := context.WithCancel(parent)
	c := &Client{ctx: ctx, cancel: cancel, protocol: protocol, mode: mode, endpoint: endpoint, path: path, auth: authorization(user, password)}
	c.h2 = &http2.Transport{TLSClientConfig: t, ReadIdleTimeout: 15 * time.Second, PingTimeout: 5 * time.Second, DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
		tc := t.Clone()
		tc.NextProtos = []string{"h2"}
		d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: tc}
		conn, err := d.DialContext(ctx, "tcp", endpoint)
		if err == nil && conn.(*tls.Conn).ConnectionState().NegotiatedProtocol != "h2" {
			conn.Close()
			return nil, errors.New("peer did not negotiate HTTP/2")
		}
		return conn, err
	}}
	c.h3 = &http3.Transport{TLSClientConfig: t, QUICConfig: quicConfig(), Dial: func(ctx context.Context, _ string, tc *tls.Config, qc *quic.Config) (*quic.Conn, error) {
		return quic.DialAddr(ctx, endpoint, tc, qc)
	}}
	c.wt = &webtransport.Transport{TLSClientConfig: t, QUICConfig: quicConfig(), ApplicationProtocols: []string{"backpack-stream-v1"}}
	context.AfterFunc(ctx, func() { c.Close() })
	return c, nil
}

func (c *Client) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.h2.CloseIdleConnections()
		c.h3.Close()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.session != nil {
			c.session.CloseWithError(0, "")
		}
		if c.wtStarted {
			c.wt.Close()
		}
	})
	return nil
}

func (c *Client) webSession(ctx context.Context) (*webtransport.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	if c.session != nil && c.session.Context().Err() == nil {
		return c.session, nil
	}
	setup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	h := http.Header{"Authorization": []string{c.auth}}
	c.wtStarted = true
	resp, sess, err := dialWebTransport(setup, c.wt, "https://"+c.endpoint+c.path, h)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, err
	}
	c.session = sess
	return sess, nil
}

func (c *Client) DialStream(ctx context.Context, target string) (io.ReadWriteCloser, error) {
	if err := FixedTarget(target); err != nil {
		return nil, err
	}
	if c.protocol == "webtransport" {
		sess, err := c.webSession(ctx)
		if err != nil {
			return nil, err
		}
		s, err := sess.OpenStreamSync(ctx)
		if err != nil {
			return nil, err
		}
		return &webStream{Stream: s}, nil
	}
	if c.mode == "h2" {
		return c.connect(ctx, target, c.h2)
	}
	if c.mode == "h3" {
		return c.connect(ctx, target, c.h3)
	}
	// Bound the race to this dial. A losing authenticated stream is always
	// closed, including when its response arrives after the winner returned.
	race, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		stream io.ReadWriteCloser
		err    error
	}
	out := make(chan result)
	attempt := func(rt http.RoundTripper, delay time.Duration) {
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-race.Done():
				return
			}
		}
		s, err := c.connect(race, target, rt)
		select {
		case out <- result{s, err}:
		case <-race.Done():
			if s != nil {
				s.Close()
			}
		}
	}
	go attempt(c.h3, 0)
	go attempt(c.h2, time.Second)
	var last error
	for range 2 {
		select {
		case r := <-out:
			if r.err == nil {
				return r.stream, nil
			}
			last = r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, last
}

func (c *Client) connect(dial context.Context, target string, rt http.RoundTripper) (io.ReadWriteCloser, error) {
	ctx, cancel := context.WithCancel(c.ctx)
	stop := context.AfterFunc(dial, cancel)
	pr, pw := io.Pipe()
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Scheme: "https", Host: c.endpoint}, Host: target, Header: make(http.Header), Body: pr, ContentLength: -1}
	req = req.WithContext(ctx)
	req.Header.Set("Proxy-Authorization", c.auth)
	req.Header.Set("User-Agent", "linux backpack")
	resp, err := rt.RoundTrip(req)
	stop()
	if err != nil {
		cancel()
		pr.Close()
		pw.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("TrustTunnel CONNECT returned HTTP %d", resp.StatusCode)
	}
	return &httpStream{Reader: resp.Body, writer: pw, cancel: cancel}, nil
}

type httpStream struct {
	io.Reader
	writer *io.PipeWriter
	cancel context.CancelFunc
	once   sync.Once
}

func (s *httpStream) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *httpStream) CloseWrite() error           { return s.writer.Close() }
func (s *httpStream) Close() error {
	s.once.Do(func() { s.cancel(); s.writer.Close(); s.Reader.(io.Closer).Close() })
	return nil
}
