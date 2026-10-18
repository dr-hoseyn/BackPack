package httpcarrier

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

type datagram struct {
	body []byte
	peer net.Addr
}

// PacketConn preserves authenticated WebTransport datagram boundaries, bounds
// the receive queue and session count, and implements changing read deadlines.
type PacketConn struct {
	ctx             context.Context
	cancel          context.CancelFunc
	local, peer     net.Addr
	mu              sync.Mutex
	sessions        map[string]*webtransport.Session
	in              chan datagram
	readAt, writeAt time.Time
	wake            chan struct{}
	closeTransport  func()
	wg              sync.WaitGroup
	once            sync.Once
	sequence        atomic.Uint32
	fragments       map[fragmentKey]*assembly
	active          int
}

func newPackets(parent context.Context) *PacketConn {
	ctx, cancel := context.WithCancel(parent)
	return &PacketConn{ctx: ctx, cancel: cancel, sessions: map[string]*webtransport.Session{}, in: make(chan datagram, 64), wake: make(chan struct{}), fragments: make(map[fragmentKey]*assembly)}
}

func ListenPackets(parent context.Context, addr, path, user, password string, cfg *tls.Config) (*PacketConn, error) {
	if err := Validate("webtransport", "h3", user, password, path); err != nil {
		return nil, err
	}
	t, err := verifiedTLS(cfg)
	if err != nil {
		return nil, err
	}
	if len(t.Certificates) == 0 {
		return nil, errors.New("WebTransport needs a server certificate")
	}
	u, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	p := newPackets(parent)
	p.local = u.LocalAddr()
	h3 := &http3.Server{TLSConfig: http3.ConfigureTLSConfig(t), QUICConfig: quicConfig(), MaxHeaderBytes: 8192}
	webtransport.ConfigureHTTP3Server(h3)
	s := &webtransport.Server{H3: h3, ApplicationProtocols: []string{"backpack-datagram-v1"}, ReorderingTimeout: 5 * time.Second}
	auth := authorization(user, password)
	h3.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r.Header.Get("Authorization"), auth) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodConnect || r.URL.Path != path || r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		p.mu.Lock()
		if p.ctx.Err() != nil || p.active >= 64 {
			p.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		p.wg.Add(1)
		p.active++
		p.mu.Unlock()
		defer p.wg.Done()
		defer func() { p.mu.Lock(); p.active--; p.mu.Unlock() }()
		sess, err := s.Upgrade(w, r)
		if err != nil {
			return
		}
		defer sess.CloseWithError(0, "")
		if sess.SessionState().ApplicationProtocol != "backpack-datagram-v1" {
			return
		}
		peer := sess.RemoteAddr()
		key := peer.String()
		p.mu.Lock()
		if p.ctx.Err() != nil || p.sessions[key] != nil {
			p.mu.Unlock()
			return
		}
		p.sessions[key] = sess
		p.mu.Unlock()
		defer func() { p.mu.Lock(); delete(p.sessions, key); p.mu.Unlock() }()
		p.receive(sess, peer)
	})
	p.closeTransport = func() { s.Close(); u.Close() }
	p.wg.Go(func() { _ = s.Serve(u); p.cancel() })
	context.AfterFunc(p.ctx, func() { p.Close() })
	return p, nil
}

func DialPackets(parent, setup context.Context, endpoint, path, user, password string, cfg *tls.Config) (*PacketConn, error) {
	if err := Validate("webtransport", "h3", user, password, path); err != nil {
		return nil, err
	}
	t, err := verifiedTLS(cfg)
	if err != nil {
		return nil, err
	}
	tr := &webtransport.Transport{TLSClientConfig: t, QUICConfig: quicConfig(), ApplicationProtocols: []string{"backpack-datagram-v1"}}
	p := newPackets(parent)
	ctx, cancel := context.WithTimeout(setup, 5*time.Second)
	defer cancel()
	stopSetup := context.AfterFunc(parent, cancel)
	defer stopSetup()
	resp, sess, err := dialWebTransport(ctx, tr, "https://"+endpoint+path, http.Header{"Authorization": []string{authorization(user, password)}})
	if err != nil {
		p.cancel()
		tr.Close()
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return nil, err
	}
	if sess.SessionState().ApplicationProtocol != "backpack-datagram-v1" {
		sess.CloseWithError(0, "")
		tr.Close()
		p.cancel()
		return nil, errors.New("peer did not negotiate Backpack datagrams")
	}
	p.local, p.peer = sess.LocalAddr(), sess.RemoteAddr()
	p.sessions[p.peer.String()] = sess
	p.closeTransport = func() { sess.CloseWithError(0, ""); tr.Close() }
	p.wg.Go(func() { p.receive(sess, p.peer); p.cancel() })
	context.AfterFunc(p.ctx, func() { p.Close() })
	return p, nil
}

func (p *PacketConn) receive(s *webtransport.Session, peer net.Addr) {
	defer func() {
		p.mu.Lock()
		for k := range p.fragments {
			if k.peer == peer.String() {
				delete(p.fragments, k)
			}
		}
		p.mu.Unlock()
	}()
	for {
		body, err := s.ReceiveDatagram(p.ctx)
		if err != nil {
			return
		}
		body = p.reassemble(peer.String(), body, time.Now())
		if body == nil {
			continue
		}
		select {
		case p.in <- datagram{body, peer}:
		default:
		}
	}
}
func (p *PacketConn) Peer() net.Addr      { return p.peer }
func (p *PacketConn) LocalAddr() net.Addr { return p.local }
func (p *PacketConn) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.cancel()
		p.mu.Unlock()
		if p.closeTransport != nil {
			p.closeTransport()
		}
	})
	p.wg.Wait()
	return nil
}
func (p *PacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		p.mu.Lock()
		at, wake := p.readAt, p.wake
		p.mu.Unlock()
		if p.ctx.Err() != nil {
			return 0, nil, net.ErrClosed
		}
		var timer *time.Timer
		var expired <-chan time.Time
		if !at.IsZero() {
			if !time.Now().Before(at) {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(at))
			expired = timer.C
		}
		select {
		case d := <-p.in:
			if timer != nil {
				timer.Stop()
			}
			return copy(b, d.body), d.peer, nil
		case <-p.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-expired:
			return 0, nil, os.ErrDeadlineExceeded
		}
	}
}
func (p *PacketConn) WriteTo(b []byte, a net.Addr) (int, error) {
	if len(b) == 0 || len(b) > MaxDatagram {
		return 0, errors.New("WebTransport packet must contain 1..65535 bytes")
	}
	if a == nil {
		return 0, errors.New("WebTransport datagram requires a peer")
	}
	p.mu.Lock()
	s, at := p.sessions[a.String()], p.writeAt
	p.mu.Unlock()
	if p.ctx.Err() != nil {
		return 0, net.ErrClosed
	}
	if !at.IsZero() && !time.Now().Before(at) {
		return 0, os.ErrDeadlineExceeded
	}
	if s == nil {
		return 0, errors.New("WebTransport peer is not authenticated")
	}
	id := p.sequence.Add(1)
	total := (len(b) + fragmentPayload - 1) / fragmentPayload
	for i := 0; i < total; i++ {
		if !at.IsZero() && !time.Now().Before(at) {
			return 0, os.ErrDeadlineExceeded
		}
		lo, hi := i*fragmentPayload, min((i+1)*fragmentPayload, len(b))
		frame := make([]byte, fragmentHeader+hi-lo)
		frame[0], frame[1], frame[2] = 1, byte(total), byte(i)
		binary.BigEndian.PutUint32(frame[4:8], id)
		binary.BigEndian.PutUint16(frame[8:10], uint16(len(b)))
		copy(frame[fragmentHeader:], b[lo:hi])
		if err := s.SendDatagram(frame); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}
func (p *PacketConn) SetDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readAt, p.writeAt = t, t
	close(p.wake)
	p.wake = make(chan struct{})
	return nil
}
func (p *PacketConn) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.readAt = t
	close(p.wake)
	p.wake = make(chan struct{})
	return nil
}
func (p *PacketConn) SetWriteDeadline(t time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writeAt = t
	return nil
}

var _ net.PacketConn = (*PacketConn)(nil)

const fragmentPayload = 1000
const fragmentHeader = 10

type fragmentKey struct {
	peer string
	id   uint32
}
type assembly struct {
	body    []byte
	seen    [2]uint64
	count   int
	expires time.Time
}

func (p *PacketConn) reassemble(peer string, b []byte, now time.Time) []byte {
	if len(b) < fragmentHeader || b[0] != 1 || b[3] != 0 {
		return nil
	}
	total, index, length := int(b[1]), int(b[2]), int(binary.BigEndian.Uint16(b[8:10]))
	if length == 0 || total != (length+fragmentPayload-1)/fragmentPayload || index >= total {
		return nil
	}
	want := min(fragmentPayload, length-index*fragmentPayload)
	if len(b) != fragmentHeader+want {
		return nil
	}
	if total == 1 {
		return append([]byte(nil), b[fragmentHeader:]...)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, a := range p.fragments {
		if !now.Before(a.expires) {
			delete(p.fragments, k)
		}
	}
	k := fragmentKey{peer, binary.BigEndian.Uint32(b[4:8])}
	a := p.fragments[k]
	if a == nil {
		if len(p.fragments) >= 32 {
			return nil
		}
		a = &assembly{body: make([]byte, length), expires: now.Add(5 * time.Second)}
		p.fragments[k] = a
	}
	if len(a.body) != length {
		return nil
	}
	word, bit := index/64, uint(index%64)
	if a.seen[word]&(1<<bit) != 0 {
		return nil
	}
	copy(a.body[index*fragmentPayload:], b[fragmentHeader:])
	a.seen[word] |= 1 << bit
	a.count++
	if a.count == total {
		delete(p.fragments, k)
		return a.body
	}
	return nil
}
