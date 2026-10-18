package l3

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	connectip "github.com/quic-go/connect-ip-go"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/yosida95/uritemplate/v3"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// CONNECT-IP is a native IP mode. Reusing the TUN/forwarder lifecycle does not
// place opaque Noise datagrams inside an invented IP envelope. QUIC/TLS and
// the authenticated CONNECT request protect the complete original IP packets.
type connectIPSession struct {
	conn  *connectip.Conn
	peer  net.Addr
	close func()
}
type connectIPAuthKey struct{}

func connectIPTemplate(authority string) *uritemplate.Template {
	return uritemplate.MustNew("https://" + authority + "/.well-known/masque/ip/")
}

func connectIPTLS(ctx context.Context, cfg Config) (*tls.Config, error) {
	_, host, err := resolveDatagramEndpoint(ctx, cfg.Addr)
	if err != nil {
		return nil, err
	}
	if cfg.Masque.ServerName != "" {
		host = cfg.Masque.ServerName
	}
	t := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: host, NextProtos: []string{http3.NextProtoH3}}
	if cfg.Masque.CAFile != "" {
		body, err := os.ReadFile(cfg.Masque.CAFile)
		if err != nil {
			return nil, err
		}
		t.RootCAs = x509.NewCertPool()
		if !t.RootCAs.AppendCertsFromPEM(body) {
			return nil, errors.New("CONNECT-IP CA contains no PEM certificates")
		}
	}
	return t, nil
}

func dialConnectIP(ctx context.Context, cfg Config) (*connectIPSession, error) {
	setup, cancel := context.WithTimeout(ctx, quicHandshakeTimeout)
	defer cancel()
	tlsCfg, err := connectIPTLS(setup, cfg)
	if err != nil {
		return nil, err
	}
	q, err := quic.DialAddr(setup, cfg.Addr, tlsCfg, quicConfig())
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(setup, func() { q.CloseWithError(0, "") })
	defer stop()
	tr := &http3.Transport{EnableDatagrams: true}
	client := tr.NewClientConn(q)
	req, err := connectip.NewRequest(setup, connectIPTemplate(cfg.Addr))
	if err != nil {
		q.CloseWithError(0, "")
		return nil, err
	}
	req.Header().Set("Authorization", masqueAuthorization(cfg.Token))
	conn, _, err := connectip.NewClientConn(client).Dial(req)
	if err != nil {
		q.CloseWithError(0, "")
		return nil, fmt.Errorf("CONNECT-IP request: %w", err)
	}
	var closed sync.Once
	cleanup := func() { closed.Do(func() { q.CloseWithError(0, ""); conn.Close(); tr.Close() }) }
	if err := configureConnectIP(setup, conn, cfg, false); err != nil {
		cleanup()
		return nil, err
	}
	if !stop() && setup.Err() != nil {
		cleanup()
		return nil, setup.Err()
	}
	return &connectIPSession{conn: conn, peer: q.RemoteAddr(), close: cleanup}, nil
}

func ipEnds(cfg Config) (netip.Addr, netip.Addr) {
	local, _, _ := strings.Cut(cfg.LocalIP, "/")
	return netip.MustParseAddr(local).Unmap(), netip.MustParseAddr(cfg.PeerIP).Unmap()
}
func configureConnectIP(ctx context.Context, c *connectip.Conn, cfg Config, server bool) error {
	local, peer := ipEnds(cfg)
	if err := c.AdvertiseRoute([]connectip.IPRoute{{StartIP: local, EndIP: local, IPProtocol: 0}}); err != nil {
		return err
	}
	if server {
		return c.AssignAddresses([]netip.Prefix{netip.PrefixFrom(peer, peer.BitLen())})
	}
	assigned, err := c.ReceiveAddressAssignment(ctx)
	if err != nil {
		return err
	}
	if len(assigned) != 1 || assigned[0].IPPrefix != netip.PrefixFrom(local, local.BitLen()) {
		return errors.New("CONNECT-IP assigned an unexpected address; host routing was not changed")
	}
	routes, err := c.Routes(ctx)
	if err != nil {
		return err
	}
	if len(routes) != 1 || routes[0].StartIP != peer || routes[0].EndIP != peer || routes[0].IPProtocol != 0 {
		return errors.New("CONNECT-IP peer route differs from the configured point-to-point peer")
	}
	return nil
}

// One authenticated connection owns the TUN at a time. Unauthenticated QUIC
// connections have bounded lifetimes and cannot evict the active peer.
func acceptConnectIP(ctx context.Context, cfg Config, ready func(net.Addr)) (*connectIPSession, error) {
	pair, err := tls.LoadX509KeyPair(cfg.Masque.CertFile, cfg.Masque.KeyFile)
	if err != nil {
		return nil, err
	}
	qc := quicConfig()
	qc.MaxIncomingStreams = 2
	ln, err := quic.ListenAddr(cfg.Addr, http3.ConfigureTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}}), qc)
	if err != nil {
		return nil, err
	}
	ready(ln.Addr())
	accepted := make(chan *connectIPSession, 1)
	var claimed sync.Mutex
	occupied := false
	srv := &http3.Server{EnableDatagrams: true, MaxHeaderBytes: 8192}
	srv.ConnContext = func(parent context.Context, q *quic.Conn) context.Context {
		timer := time.AfterFunc(quicHandshakeTimeout, func() { q.CloseWithError(0, "authentication timeout") })
		context.AfterFunc(parent, func() { timer.Stop() })
		return context.WithValue(parent, connectIPAuthKey{}, timer)
	}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(masqueAuthorization(cfg.Token))) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		req, err := connectip.ParseProxyRequest(r, connectIPTemplate(r.Host))
		if err != nil {
			http.Error(w, "invalid CONNECT-IP request", http.StatusBadRequest)
			return
		}
		claimed.Lock()
		if occupied {
			claimed.Unlock()
			http.Error(w, "tunnel already occupied", http.StatusConflict)
			return
		}
		occupied = true
		claimed.Unlock()
		if timer, ok := r.Context().Value(connectIPAuthKey{}).(*time.Timer); ok {
			timer.Stop()
		}
		conn, err := (&connectip.Proxy{}).Proxy(w, req)
		if err != nil {
			claimed.Lock()
			occupied = false
			claimed.Unlock()
			return
		}
		if err := configureConnectIP(r.Context(), conn, cfg, true); err != nil {
			claimed.Lock()
			occupied = false
			claimed.Unlock()
			conn.Close()
			return
		}
		s := &connectIPSession{conn: conn, peer: connectIPAddr(r.RemoteAddr), close: func() { srv.Close(); ln.Close(); conn.Close() }}
		select {
		case accepted <- s:
		case <-ctx.Done():
			conn.Close()
			return
		}
		<-r.Context().Done()
	})
	var wg sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { ln.Close(); srv.Close() })
	// The accept loop is stopped before waiting for its bounded workers.
	done := make(chan struct{})
	go func() {
		defer close(done)
		slots := make(chan struct{}, 16)
		for {
			q, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				q.CloseWithError(0, "capacity")
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots; q.CloseWithError(0, "") }()
				_ = srv.ServeQUICConn(q)
			}()
		}
	}()
	cleanup := func() { stop(); ln.Close(); srv.Close(); <-done; wg.Wait() }
	select {
	case s := <-accepted:
		old := s.close
		var once sync.Once
		s.close = func() { once.Do(func() { old(); cleanup() }) }
		return s, nil
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-done:
		cleanup()
		return nil, errors.New("CONNECT-IP listener closed")
	}
}
func connectIPAddr(s string) net.Addr { a, _ := net.ResolveUDPAddr("udp", s); return a }

// Validates the complete packet and restricts traffic to the configured peers.
// Capsule updates never install routes, DNS, NAT, default routes or sysctls.
func connectIPPacket(p []byte, src, dst netip.Addr) bool {
	if len(p) < 20 {
		return false
	}
	if p[0]>>4 == 4 {
		h := int(p[0]&15) * 4
		return src.Is4() && dst.Is4() && h >= 20 && len(p) >= h && int(binary.BigEndian.Uint16(p[2:4])) == len(p) && netip.AddrFrom4([4]byte(p[12:16])) == src && netip.AddrFrom4([4]byte(p[16:20])) == dst
	}
	return p[0]>>4 == 6 && src.Is6() && dst.Is6() && len(p) >= 40 && 40+int(binary.BigEndian.Uint16(p[4:6])) == len(p) && netip.AddrFrom16([16]byte(p[8:24])) == src && netip.AddrFrom16([16]byte(p[24:40])) == dst
}

func (t *Tunnel) runConnectIP(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var s *connectIPSession
	var err error
	if t.cfg.Mode == ModeDial {
		s, err = dialConnectIP(ctx, t.cfg)
	} else {
		s, err = acceptConnectIP(ctx, t.cfg, func(a net.Addr) { t.localAddrMu.Lock(); t.localAddr = a; t.localAddrMu.Unlock() })
	}
	if err != nil {
		return err
	}
	defer s.close()
	t.stats.handshakes.Add(1)
	t.setPeer(s.peer)
	open := t.openDevice
	if open == nil {
		open = openDevice
	}
	dev, err := open(t.deviceSpecFor())
	if err != nil {
		return err
	}
	defer dev.Close()
	stop := context.AfterFunc(ctx, func() { s.close(); dev.Close() })
	defer stop()
	defer func() { t.connectIPLast.Store(0); metrics.ClearPeer() }()
	local, peer := ipEnds(t.cfg)
	var wg sync.WaitGroup
	var writes sync.Mutex
	write := func(p []byte) { writes.Lock(); defer writes.Unlock(); _, _ = dev.Write([][]byte{p}) }
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		bufs := make([][]byte, dev.BatchSize())
		sizes := make([]int, len(bufs))
		for i := range bufs {
			bufs[i] = make([]byte, tunReadBuf)
		}
		for {
			n, err := dev.Read(bufs, sizes)
			if err != nil {
				return
			}
			for i := 0; i < n; i++ {
				p := bufs[i][:sizes[i]]
				if !connectIPPacket(p, local, peer) {
					t.stats.dropped.Add(1)
					continue
				}
				reply, err := s.conn.WritePacket(p)
				if err != nil {
					return
				}
				if reply != nil {
					write(reply)
					t.stats.dropped.Add(1)
					continue
				}
				t.stats.bytesOut.Add(uint64(len(p)))
				t.stats.packetsOut.Add(1)
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		p := make([]byte, 65536)
		for {
			n, err := s.conn.ReadPacket(p)
			if err != nil {
				return
			}
			packet := p[:n]
			if !connectIPPacket(packet, peer, local) {
				t.stats.dropped.Add(1)
				continue
			}
			t.connectIPLast.Store(time.Now().UnixNano())
			t.publishPeer()
			t.stats.bytesIn.Add(uint64(n))
			t.stats.packetsIn.Add(1)
			write(packet)
		}
	}()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	// Kernel ICMP replies prove the complete TUN path in both directions.
	started := time.Now()
	for ctx.Err() == nil {
		last := t.connectIPLast.Load()
		if (last == 0 && time.Since(started) > 30*time.Second) || (last != 0 && !t.Up()) {
			cancel()
			break
		}
		if _, err := s.conn.WritePacket(connectIPPing(local, peer)); err != nil {
			cancel()
			break
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	s.close()
	dev.Close()
	wg.Wait()
	return ctx.Err()
}

func connectIPPing(src, dst netip.Addr) []byte {
	msg := icmp.Message{Code: 0, Body: &icmp.Echo{ID: 0x4250, Seq: int(time.Now().Unix() % 65536), Data: []byte("backpack-connect-ip")}}
	if src.Is4() {
		msg.Type = ipv4.ICMPTypeEcho
		body, _ := msg.Marshal(nil)
		h := &ipv4.Header{Version: 4, Len: 20, TotalLen: 20 + len(body), TTL: 64, Protocol: 1, Src: net.IP(src.AsSlice()), Dst: net.IP(dst.AsSlice())}
		b, _ := h.Marshal()
		binary.BigEndian.PutUint16(b[10:12], connectIPChecksum(b))
		return append(b, body...)
	}
	msg.Type = ipv6.ICMPTypeEchoRequest
	body, _ := msg.Marshal(icmp.IPv6PseudoHeader(src.AsSlice(), dst.AsSlice()))
	b := make([]byte, 40)
	b[0] = 0x60
	b[6] = 58
	b[7] = 64
	binary.BigEndian.PutUint16(b[4:6], uint16(len(body)))
	copy(b[8:24], src.AsSlice())
	copy(b[24:40], dst.AsSlice())
	return append(b, body...)
}
func connectIPChecksum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b))
		b = b[2:]
	}
	if len(b) > 0 {
		sum += uint32(b[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}
