package l3

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	masque "github.com/quic-go/masque-go"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/yosida95/uritemplate/v3"
)

// MASQUE adds HTTP Datagram framing (quarter stream ID and context ID) to
// QUIC DATAGRAM. Reserve IPv6 outer headers and the largest varint IDs too.
const masqueOverhead = quicOverhead + 20 + 16

type masqueAuthenticationKey struct{}

type masqueCarrier struct {
	net.PacketConn
	local    net.Addr
	peer     net.Addr
	shutdown func()
	once     sync.Once
	done     chan struct{}
	in       chan quicDatagram
	mu       sync.Mutex
	terminal error
	readAt   time.Time
	wake     chan struct{}
	// Only authenticated proxy sockets may exchange packets with the backend.
	allowed map[string]bool
}

func masqueAuthorization(token string) string {
	sum := sha256.Sum256([]byte("backpack masque authorization v1\x00" + token))
	return "Bearer " + hex.EncodeToString(sum[:])
}

func masqueTemplate(authority string) *uritemplate.Template {
	return uritemplate.MustNew("https://" + authority + "/.well-known/masque/udp/{target_host}/{target_port}/")
}

func openMasque(ctx context.Context, cfg Config) (DatagramCarrier, net.Addr, error) {
	if err := cfg.Masque.Validate(cfg.Mode == ModeListen); err != nil {
		return nil, nil, fmt.Errorf("l3: masque: %w", err)
	}
	if cfg.Mode == ModeListen {
		return listenMasque(ctx, cfg)
	}
	return dialMasque(ctx, cfg)
}

func dialMasque(ctx context.Context, cfg Config) (DatagramCarrier, net.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, quicHandshakeTimeout)
	defer cancel()
	peer, host, err := resolveDatagramEndpoint(ctx, cfg.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("l3: masque: resolving endpoint: %w", err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{http3.NextProtoH3}, ServerName: host}
	if cfg.Masque.ServerName != "" {
		tlsCfg.ServerName = cfg.Masque.ServerName
	}
	if cfg.Masque.CAFile != "" {
		pem, err := os.ReadFile(cfg.Masque.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("l3: masque: reading CA: %w", err)
		}
		tlsCfg.RootCAs = x509.NewCertPool()
		if !tlsCfg.RootCAs.AppendCertsFromPEM(pem) {
			return nil, nil, errors.New("l3: masque: CA file contains no certificates")
		}
	}
	qconn, err := quic.DialAddr(ctx, peer.String(), tlsCfg, quicConfig())
	if err != nil {
		return nil, nil, fmt.Errorf("l3: masque: TLS/QUIC handshake: %w", err)
	}
	// masque-go bounds opening the request with ctx, but ReadResponse can
	// otherwise wait forever after the request headers have been sent.
	stop := context.AfterFunc(ctx, func() { qconn.CloseWithError(0, "setup cancelled") })
	defer stop()
	tr := &masque.Transport{}
	client, err := tr.NewClientConn(qconn)
	if err != nil {
		qconn.CloseWithError(0, "")
		return nil, nil, err
	}
	req, err := masque.NewRequest(ctx, masqueTemplate(cfg.Addr), cfg.Masque.TargetAddr())
	if err != nil {
		qconn.CloseWithError(0, "")
		return nil, nil, err
	}
	req.Header().Set("Authorization", masqueAuthorization(cfg.Token))
	conn, _, err := client.Dial(req)
	if err != nil {
		qconn.CloseWithError(0, "")
		return nil, nil, fmt.Errorf("l3: masque: CONNECT-UDP: %w", err)
	}
	stop()
	if err := ctx.Err(); err != nil {
		conn.Close()
		qconn.CloseWithError(0, "")
		return nil, nil, err
	}
	c := newMasqueCarrier(conn, conn.LocalAddr())
	c.peer = peer
	c.shutdown = func() { qconn.CloseWithError(0, ""); _ = conn.Close() }
	go c.receive()
	return c, peer, nil
}

func listenMasque(ctx context.Context, cfg Config) (DatagramCarrier, net.Addr, error) {
	return listenMasqueWithAuthTimeout(ctx, cfg, quicHandshakeTimeout)
}

func listenMasqueWithAuthTimeout(ctx context.Context, cfg Config, authTimeout time.Duration) (DatagramCarrier, net.Addr, error) {
	pair, err := tls.LoadX509KeyPair(cfg.Masque.CertFile, cfg.Masque.KeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("l3: masque: loading TLS certificate: %w", err)
	}
	backendAddr, _ := net.ResolveUDPAddr("udp", cfg.Masque.TargetAddr()) // validated numeric address
	backend, err := net.ListenUDP("udp", backendAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("l3: masque: private UDP target: %w", err)
	}
	addr, _, err := resolveDatagramEndpoint(ctx, cfg.Addr)
	if err != nil {
		backend.Close()
		return nil, nil, err
	}
	udp, err := net.ListenUDP("udp", addr)
	if err != nil {
		backend.Close()
		return nil, nil, err
	}
	sizeUDPBuffers(udp, cfg.SockBuf)
	tr := &quic.Transport{Conn: udp, StatelessResetKey: quicResetKey(cfg.Token)}
	qc := quicConfig()
	qc.MaxIncomingStreams = 8
	qc.HandshakeIdleTimeout = quicHandshakeTimeout
	tlsCfg := http3.ConfigureTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}})
	ln, err := tr.Listen(tlsCfg, qc)
	if err != nil {
		tr.Close()
		udp.Close()
		backend.Close()
		return nil, nil, err
	}
	c := newMasqueCarrier(backend, ln.Addr())
	c.allowed = make(map[string]bool)
	proxy := &masque.Proxy{}
	slots := make(chan struct{}, quicMaxPeers)
	auth := masqueAuthorization(cfg.Token)
	srv := &http3.Server{EnableDatagrams: true, MaxHeaderBytes: 8192,
		ConnContext: func(ctx context.Context, conn *quic.Conn) context.Context {
			// QUIC keepalives can keep an unauthenticated connection alive
			// forever. Bound the time it may occupy a connection slot.
			timer := time.AfterFunc(authTimeout, func() {
				conn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeRequestRejected), "authentication timeout")
			})
			context.AfterFunc(ctx, func() { timer.Stop() })
			return context.WithValue(ctx, masqueAuthenticationKey{}, timer)
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(auth)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			template, err := uritemplate.New("https://" + r.Host + "/.well-known/masque/udp/{target_host}/{target_port}/")
			if err != nil {
				http.Error(w, "invalid authority", http.StatusBadRequest)
				return
			}
			req, err := masque.ParseProxyRequest(r, template)
			if err != nil {
				http.Error(w, "invalid CONNECT-UDP request", http.StatusBadRequest)
				return
			}
			if req.Target != cfg.Masque.TargetAddr() {
				http.Error(w, "target not allowed", http.StatusForbidden)
				return
			}
			if timer, ok := r.Context().Value(masqueAuthenticationKey{}).(*time.Timer); ok {
				timer.Stop()
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				http.Error(w, "too many tunnels", http.StatusServiceUnavailable)
				return
			}
			sock, err := net.DialUDP("udp", nil, backendAddr)
			if err != nil {
				http.Error(w, "backend unavailable", http.StatusBadGateway)
				return
			}
			defer sock.Close()
			key := sock.LocalAddr().String()
			c.mu.Lock()
			c.allowed[key] = true
			c.mu.Unlock()
			defer func() { c.mu.Lock(); delete(c.allowed, key); c.mu.Unlock() }()
			_ = proxy.ProxyConnectedSocket(w, req, sock)
		})}
	acceptDone := make(chan struct{})
	var workers sync.WaitGroup
	connections := make(chan struct{}, quicMaxPeers)
	c.shutdown = func() {
		ln.Close()
		<-acceptDone // no further workers.Add calls after this
		srv.Close()
		tr.Close()
		udp.Close()
		proxy.Close()
		workers.Wait()
		backend.Close()
	}
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				go c.Close()
				return
			}
			select {
			case connections <- struct{}{}:
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer func() { <-connections; conn.CloseWithError(0, "") }()
					_ = srv.ServeQUICConn(conn)
				}()
			default:
				conn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeExcessiveLoad), "capacity")
			}
		}
	}()
	go c.receive()
	return c, nil, nil
}

func newMasqueCarrier(conn net.PacketConn, local net.Addr) *masqueCarrier {
	return &masqueCarrier{PacketConn: conn, local: local, done: make(chan struct{}),
		in: make(chan quicDatagram, quicInbox), wake: make(chan struct{})}
}

func (c *masqueCarrier) receive() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := c.PacketConn.ReadFrom(buf)
		if err != nil {
			c.mu.Lock()
			c.terminal = err
			c.mu.Unlock()
			go c.Close()
			return
		}
		if c.peer != nil {
			addr = c.peer
		} else {
			c.mu.Lock()
			ok := c.allowed[addr.String()]
			c.mu.Unlock()
			if !ok {
				continue
			}
		}
		d := quicDatagram{data: append([]byte(nil), buf[:n]...), from: addr}
		select {
		case c.in <- d:
		case <-c.done:
			return
		default: // datagrams may be dropped; never block shutdown on a full inbox
		}
	}
}

func (c *masqueCarrier) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		c.mu.Lock()
		at, wake := c.readAt, c.wake
		c.mu.Unlock()
		var deadline <-chan time.Time
		var timer *time.Timer
		if !at.IsZero() {
			if !time.Now().Before(at) {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(at))
			deadline = timer.C
		}
		select {
		case d := <-c.in:
			if timer != nil {
				timer.Stop()
			}
			return copy(p, d.data), d.from, nil
		case <-deadline:
			return 0, nil, os.ErrDeadlineExceeded
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			c.mu.Lock()
			err := c.terminal
			c.mu.Unlock()
			if err == nil {
				err = net.ErrClosed
			}
			return 0, nil, err
		}
	}
}

func (c *masqueCarrier) WriteTo(p []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	if c.peer == nil {
		if addr == nil {
			return 0, errNoDialler
		}
		c.mu.Lock()
		ok := c.allowed[addr.String()]
		c.mu.Unlock()
		if !ok {
			return 0, errNoDialler
		}
	}
	return c.PacketConn.WriteTo(p, addr)
}

func (c *masqueCarrier) CarrierName() string { return CarrierMasque }
func (c *masqueCarrier) Overhead() int       { return masqueOverhead }
func (c *masqueCarrier) LocalAddr() net.Addr { return c.local }
func (c *masqueCarrier) Close() error {
	c.once.Do(func() { close(c.done); c.shutdown() })
	return nil
}
func (c *masqueCarrier) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readAt = t
	close(c.wake)
	c.wake = make(chan struct{})
	c.mu.Unlock()
	return nil
}
func (c *masqueCarrier) SetWriteDeadline(t time.Time) error { return c.PacketConn.SetWriteDeadline(t) }
func (c *masqueCarrier) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}
