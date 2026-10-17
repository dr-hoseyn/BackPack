//go:build !windows

package adaptive

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/backpack/backpack/internal/tunnel/naive"
)

// packetConn preserves one Noise session while native datagrams use the
// selected healthy association. Outage packets are lost. Associations on the
// other paths remain warm and their replies are merged without stream framing.
type packetConn struct {
	parent          *ClientPaths
	ctx             context.Context
	cancel          context.CancelFunc
	once            sync.Once
	wg              sync.WaitGroup
	mu              sync.Mutex
	pcs             map[int]*naive.SOCKSPacketConn
	peer            *net.UDPAddr
	in              chan []byte
	readAt, writeAt time.Time
	wake            chan struct{}
}

func (c *ClientPaths) PacketConn() (net.PacketConn, net.Addr, error) {
	if !c.cfg.NativeSing().NativeUDP {
		return nil, nil, errors.New("no native UDP path configured")
	}
	peer, err := net.ResolveUDPAddr("udp", c.cfg.RemoteAddr)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(c.ctx)
	p := &packetConn{parent: c, ctx: ctx, cancel: cancel, pcs: map[int]*naive.SOCKSPacketConn{}, peer: peer, in: make(chan []byte, 128), wake: make(chan struct{})}
	for i, path := range c.cfg.Paths {
		if path.Sing.NativeUDP {
			p.wg.Add(1)
			go p.association(i)
		}
	}
	c.mu.Lock()
	c.packets = append(c.packets, p)
	c.mu.Unlock()
	context.AfterFunc(ctx, func() { p.Close() })
	return p, peer, nil
}

func (p *packetConn) association(i int) {
	defer p.wg.Done()
	for p.ctx.Err() == nil {
		out, err := p.parent.outbound(i)
		if err == nil {
			var pc *naive.SOCKSPacketConn
			pc, err = naive.NewSOCKSPacketConn(p.ctx, out.Proxy.Address, p.parent.cfg.RemoteAddr)
			if err == nil {
				p.mu.Lock()
				p.pcs[i] = pc
				p.mu.Unlock()
				stop := context.AfterFunc(p.ctx, func() { pc.Close() })
				b := make([]byte, 65535)
				for {
					n, _, readErr := pc.ReadFrom(b)
					if readErr != nil {
						break
					}
					select {
					case p.in <- append([]byte(nil), b[:n]...):
					default:
					}
				}
				stop()
				pc.Close()
				p.mu.Lock()
				delete(p.pcs, i)
				p.mu.Unlock()
			}
		}
		t := time.NewTimer(time.Second)
		select {
		case <-p.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (p *packetConn) Close() error {
	p.once.Do(func() {
		p.cancel()
		p.mu.Lock()
		var conns []*naive.SOCKSPacketConn
		for _, pc := range p.pcs {
			conns = append(conns, pc)
		}
		p.mu.Unlock()
		for _, pc := range conns {
			pc.Close()
		}
		p.wg.Wait()
	})
	return nil
}
func (p *packetConn) LocalAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (p *packetConn) WriteTo(b []byte, a net.Addr) (int, error) {
	if a == nil || a.String() != p.peer.String() {
		return 0, errors.New("native managed path target is fixed")
	}
	if p.ctx.Err() != nil {
		return 0, net.ErrClosed
	}
	i := p.parent.selector.Pick(true)
	p.mu.Lock()
	pc, deadline := p.pcs[i], p.writeAt
	p.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if pc == nil {
		return len(b), nil
	}
	pc.SetWriteDeadline(deadline)
	return pc.WriteTo(b, p.peer)
}

func (p *packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		p.mu.Lock()
		deadline, wake := p.readAt, p.wake
		p.mu.Unlock()
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
		case body := <-p.in:
			if timer != nil {
				timer.Stop()
			}
			if len(body) > len(b) {
				return 0, nil, errors.New("native path receive buffer too small")
			}
			return copy(b, body), p.peer, nil
		case <-p.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		case <-expired:
			return 0, nil, os.ErrDeadlineExceeded
		case <-wake:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}
func (p *packetConn) SetDeadline(t time.Time) error {
	p.SetReadDeadline(t)
	return p.SetWriteDeadline(t)
}
func (p *packetConn) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	p.readAt = t
	close(p.wake)
	p.wake = make(chan struct{})
	p.mu.Unlock()
	return nil
}
func (p *packetConn) SetWriteDeadline(t time.Time) error {
	p.mu.Lock()
	p.writeAt = t
	var conns []*naive.SOCKSPacketConn
	for _, pc := range p.pcs {
		conns = append(conns, pc)
	}
	p.mu.Unlock()
	for _, pc := range conns {
		pc.SetWriteDeadline(t)
	}
	return nil
}
