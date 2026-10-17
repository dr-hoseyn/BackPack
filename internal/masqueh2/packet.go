package masqueh2

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2/hpack"
)

type packet struct {
	body []byte
	from net.Addr
}

type writeOperation struct {
	cancel context.CancelFunc
	timer  *time.Timer
}

// Conn owns its listener, all authenticated streams, and its bounded receive
// queue. Close waits for every worker, so a reload can immediately reuse ports.
type Conn struct {
	ctx             context.Context
	cancel          context.CancelFunc
	once            sync.Once
	wg              sync.WaitGroup
	listener        net.Listener
	local           net.Addr
	peer            net.Addr
	mu              sync.Mutex
	sessions        map[string]*session
	pending         map[net.Conn]bool
	in              chan packet
	readAt, writeAt time.Time
	wake            chan struct{}
	terminal        error
	writes          map[*writeOperation]bool
	dropped         atomic.Uint64
}

func newConn(ctx context.Context) *Conn {
	ctx, cancel := context.WithCancel(ctx)
	c := &Conn{ctx: ctx, cancel: cancel, sessions: make(map[string]*session), pending: make(map[net.Conn]bool), in: make(chan packet, 64), wake: make(chan struct{}), writes: make(map[*writeOperation]bool)}
	context.AfterFunc(ctx, func() { c.Close() })
	return c
}

func tlsConfig(cfg *tls.Config) (*tls.Config, error) {
	if cfg == nil || cfg.InsecureSkipVerify {
		return nil, errors.New("MASQUE/H2 requires verified TLS")
	}
	cfg = cfg.Clone()
	cfg.MinVersion = tls.VersionTLS13
	cfg.NextProtos = []string{"h2"}
	return cfg, nil
}

func targetPath(target string) (string, error) {
	host, service, err := net.SplitHostPort(target)
	ip := net.ParseIP(host)
	port, pe := strconv.Atoi(service)
	if err != nil || pe != nil || ip == nil || !ip.IsLoopback() || port < 1 || port > 65535 {
		return "", errors.New("MASQUE/H2 target must be a literal loopback IP:port")
	}
	return "/.well-known/masque/udp/" + url.PathEscape(ip.String()) + "/" + strconv.Itoa(port) + "/", nil
}

func authorization(token string) string {
	sum := sha256.Sum256([]byte("backpack masque h2 authorization v1\x00" + token))
	return "Bearer " + hex.EncodeToString(sum[:])
}

func (c *Conn) session(conn net.Conn, server bool, headers []hpack.HeaderField, validate func([]hpack.HeaderField) error) *session {
	a := conn.RemoteAddr()
	s := newSession(c.ctx, conn, server, headers, validate, func(b []byte) {
		pkt := packet{body: append([]byte(nil), b...), from: a}
		select {
		case c.in <- pkt:
		default:
			c.dropped.Add(1)
		} // A slow TUN drops packets instead of growing memory.
	})
	c.mu.Lock()
	c.sessions[a.String()] = s
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		s.run()
		s.mu.Lock()
		terminal := s.terminal
		s.mu.Unlock()
		c.mu.Lock()
		delete(c.sessions, a.String())
		c.terminal = terminal
		c.mu.Unlock()
		if !server {
			c.cancel()
		}
	}()
	return s
}

func Dial(ctx context.Context, addr, target, token string, cfg *tls.Config) (*Conn, error) {
	path, err := targetPath(target)
	if err != nil {
		return nil, err
	}
	if len(token) < 16 {
		return nil, errors.New("MASQUE/H2 requires a strong shared token")
	}
	cfg, err = tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	setup, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, err := (&tls.Dialer{Config: cfg}).DialContext(setup, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if conn.(*tls.Conn).ConnectionState().NegotiatedProtocol != "h2" {
		conn.Close()
		return nil, errors.New("peer did not negotiate HTTP/2")
	}
	c := newConn(ctx)
	c.local, c.peer = conn.LocalAddr(), conn.RemoteAddr()
	s := c.session(conn, false, []hpack.HeaderField{
		{Name: ":method", Value: "CONNECT"}, {Name: ":protocol", Value: "connect-udp"}, {Name: ":scheme", Value: "https"}, {Name: ":authority", Value: addr}, {Name: ":path", Value: path},
		{Name: "capsule-protocol", Value: "?1"}, {Name: "authorization", Value: authorization(token), Sensitive: true},
	}, nil)
	select {
	case err := <-s.ready:
		if err != nil {
			c.Close()
			return nil, err
		}
	case <-setup.Done():
		c.Close()
		return nil, setup.Err()
	}
	return c, nil
}

func Listen(ctx context.Context, addr, target, token string, cfg *tls.Config) (*Conn, error) {
	path, err := targetPath(target)
	if err != nil {
		return nil, err
	}
	if len(token) < 16 {
		return nil, errors.New("MASQUE/H2 requires a strong shared token")
	}
	cfg, err = tlsConfig(cfg)
	if err != nil {
		return nil, err
	}
	l, err := tls.Listen("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	c := newConn(ctx)
	c.local, c.listener = l.Addr(), l
	validate := func(fields []hpack.HeaderField) error {
		values := make(map[string]string, len(fields))
		for _, f := range fields {
			if _, duplicate := values[f.Name]; duplicate {
				return errors.New("duplicate CONNECT header")
			}
			values[f.Name] = f.Value
		}
		got, want := []byte(values["authorization"]), []byte(authorization(token))
		if values[":method"] != "CONNECT" || values[":protocol"] != "connect-udp" || values[":scheme"] != "https" || values[":authority"] == "" || values[":path"] != path || values["capsule-protocol"] != "?1" || subtle.ConstantTimeCompare(got, want) != 1 {
			return errors.New("CONNECT-UDP authentication or target refused")
		}
		return nil
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		slots := make(chan struct{}, 16)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			c.mu.Lock()
			c.pending[conn] = true
			c.mu.Unlock()
			c.wg.Add(1)
			go func() {
				defer c.wg.Done()
				defer func() { <-slots; conn.Close(); c.mu.Lock(); delete(c.pending, conn); c.mu.Unlock() }()
				setup, cancel := context.WithTimeout(c.ctx, 12*time.Second)
				tlsConn := conn.(*tls.Conn)
				err := tlsConn.HandshakeContext(setup)
				cancel()
				if err != nil || tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
					return
				}
				s := c.session(conn, true, nil, validate)
				<-s.done
			}()
		}
	}()
	return c, nil
}

func (c *Conn) Peer() net.Addr      { return c.peer }
func (c *Conn) LocalAddr() net.Addr { return c.local }

func (c *Conn) Close() error {
	c.once.Do(func() {
		c.cancel()
		if c.listener != nil {
			c.listener.Close()
		}
		c.mu.Lock()
		for conn := range c.pending {
			conn.Close()
		}
		for _, s := range c.sessions {
			s.close()
		}
		c.mu.Unlock()
		c.wg.Wait()
	})
	return nil
}

func (c *Conn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		c.mu.Lock()
		deadline, wake := c.readAt, c.wake
		c.mu.Unlock()
		var timer *time.Timer
		var expired <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			expired = timer.C
		}
		select {
		case p := <-c.in:
			if timer != nil {
				timer.Stop()
			}
			if len(b) < len(p.body) {
				return 0, p.from, errors.New("MASQUE datagram receive buffer too small")
			}
			return copy(b, p.body), p.from, nil
		case <-c.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			c.mu.Lock()
			err := c.terminal
			c.mu.Unlock()
			return 0, nil, errors.Join(net.ErrClosed, err)
		case <-expired:
			return 0, nil, os.ErrDeadlineExceeded
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

func (c *Conn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if addr == nil {
		return 0, errors.New("missing MASQUE peer")
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	op := &writeOperation{cancel: cancel}
	c.mu.Lock()
	s, deadline := c.sessions[addr.String()], c.writeAt
	if !deadline.IsZero() {
		op.timer = time.AfterFunc(time.Until(deadline), cancel)
	}
	c.writes[op] = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.writes, op)
		if op.timer != nil {
			op.timer.Stop()
		}
		c.mu.Unlock()
	}()
	if s == nil {
		return 0, net.ErrClosed
	}
	if !deadline.IsZero() {
		if !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}
	}
	if err := s.send(ctx, b); err != nil {
		if ctx.Err() != nil && c.ctx.Err() == nil {
			return 0, os.ErrDeadlineExceeded
		}
		return 0, err
	}
	return len(b), nil
}

func (c *Conn) SetDeadline(t time.Time) error { _ = c.SetReadDeadline(t); return c.SetWriteDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readAt = t
	close(c.wake)
	c.wake = make(chan struct{})
	c.mu.Unlock()
	return nil
}
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeAt = t
	for op := range c.writes {
		if op.timer != nil {
			op.timer.Stop()
		}
		if !t.IsZero() {
			op.timer = time.AfterFunc(time.Until(t), op.cancel)
		} else {
			op.timer = nil
		}
	}
	c.mu.Unlock()
	return nil
}
