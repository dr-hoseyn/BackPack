package nativeudp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/backpack/backpack/internal/metrics"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/backpack/backpack/internal/tunnel/portmap"
	"golang.org/x/time/rate"
)

const idle = 60 * time.Second

type flow struct {
	id     uint64
	target string
	entry  *net.UDPConn
	peer   *net.UDPAddr
	last   atomic.Int64
	queue  chan []byte
}

// Engine bounds flow count, per-flow pending datagrams and bandwidth. Outage
// packets and over-budget packets are dropped, never retransmitted or retained
// until a new helper process appears.
type Engine struct {
	ctx               context.Context
	cancel            context.CancelFunc
	link              *Link
	server            bool
	max               int
	limiter           *rate.Limiter
	mu                sync.Mutex
	flows             map[uint64]*flow
	sources           map[string]*flow
	sockets           []*net.UDPConn
	next              uint64
	wg                sync.WaitGroup
	once              sync.Once
	unregister        func()
	drops             atomic.Uint64
	bytesIn, bytesOut atomic.Uint64
}

type EngineStats struct {
	Stats
	Flows    int    `json:"flows"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
}

func newEngine(parent context.Context, pc net.PacketConn, peer net.Addr, token string, maxFlows, mbps int) (*Engine, error) {
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	if maxFlows == 0 {
		maxFlows = 128
	}
	if maxFlows < 1 || maxFlows > 4096 || mbps < 0 || mbps > 100000 {
		return nil, errors.New("native UDP limits require 1..4096 flows and 0..100000 Mbit/s")
	}
	ctx, cancel := context.WithCancel(parent)
	link, err := NewLink(ctx, pc, peer, token)
	if err != nil {
		cancel()
		return nil, err
	}
	e := &Engine{ctx: ctx, cancel: cancel, link: link, max: maxFlows, flows: map[uint64]*flow{}, sources: map[string]*flow{}, server: peer == nil}
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		link.Close()
		cancel()
		return nil, err
	}
	e.next = binary.BigEndian.Uint64(seed[:])
	if mbps > 0 {
		e.limiter = rate.NewLimiter(rate.Limit(float64(mbps)*1e6/8), maxPayload*2)
	}
	return e, nil
}

func StartServer(parent context.Context, pc net.PacketConn, token string, ports []string, maxFlows, mbps int) (*Engine, error) {
	mappings, err := portmap.Expand(ports, "127.0.0.1")
	if err != nil {
		return nil, err
	}
	e, err := newEngine(parent, pc, nil, token, maxFlows, mbps)
	if err != nil {
		return nil, err
	}
	for _, m := range mappings {
		addr, err := net.ResolveUDPAddr("udp", m.Listen)
		if err != nil {
			e.Close()
			return nil, err
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			e.Close()
			return nil, fmt.Errorf("native forwarded UDP %s: %w", m.Listen, err)
		}
		e.sockets = append(e.sockets, conn)
	}
	for i, m := range mappings {
		e.wg.Add(1)
		go e.entry(e.sockets[i], m.Targets)
	}
	e.start()
	return e, nil
}

func StartClient(parent context.Context, pc net.PacketConn, peer net.Addr, token string, maxFlows, mbps int) (*Engine, error) {
	if peer == nil {
		return nil, errors.New("native UDP client requires a fixed peer")
	}
	e, err := newEngine(parent, pc, peer, token, maxFlows, mbps)
	if err != nil {
		return nil, err
	}
	e.start()
	return e, nil
}

func (e *Engine) start() {
	e.unregister = metrics.RegisterNativeUDP(func() metrics.NativeUDPStats {
		s := e.Stats()
		return metrics.NativeUDPStats{Connected: s.Connected, Flows: s.Flows, Sent: s.Sent, Received: s.Received, Dropped: s.Dropped, BytesIn: s.BytesIn, BytesOut: s.BytesOut}
	})
	e.wg.Add(2)
	go e.receive()
	go e.reap()
	context.AfterFunc(e.ctx, func() { e.Close() })
}

func (e *Engine) Close() error {
	e.once.Do(func() {
		e.cancel()
		for _, s := range e.sockets {
			s.Close()
		}
		e.link.Close()
		e.wg.Wait()
		if e.unregister != nil {
			e.unregister()
		}
	})
	return nil
}

func (e *Engine) Stats() EngineStats {
	s := e.link.Stats()
	s.Dropped += e.drops.Load()
	e.mu.Lock()
	flows := len(e.flows)
	e.mu.Unlock()
	return EngineStats{s, flows, e.bytesIn.Load(), e.bytesOut.Load()}
}

func (e *Engine) allowed(n int) bool { return e.limiter == nil || e.limiter.AllowN(time.Now(), n) }

func (e *Engine) write(id uint64, target string, p []byte) {
	if len(p)+len(target)+10 > maxPayload || !e.allowed(len(p)) {
		e.drops.Add(1)
		return
	}
	b := make([]byte, 10+len(target)+len(p))
	binary.BigEndian.PutUint64(b, id)
	binary.BigEndian.PutUint16(b[8:], uint16(len(target)))
	copy(b[10:], target)
	copy(b[10+len(target):], p)
	if err := e.link.Write(b); err == nil {
		e.bytesOut.Add(uint64(len(p)))
		metrics.AddBytes(0, uint64(len(p)))
	}
}

func (e *Engine) entry(conn *net.UDPConn, targets []string) {
	defer e.wg.Done()
	b := make([]byte, 65535)
	var nextTarget uint64
	for {
		n, from, err := conn.ReadFromUDP(b)
		if err != nil {
			return
		}
		if !e.link.Stats().Connected {
			e.drops.Add(1)
			continue
		}
		key := conn.LocalAddr().String() + "/" + from.String()
		e.mu.Lock()
		f := e.sources[key]
		if f == nil && len(e.flows) < e.max {
			e.next++
			if e.next == 0 {
				e.next++
			}
			f = &flow{id: e.next, target: targets[nextTarget%uint64(len(targets))], entry: conn, peer: from}
			nextTarget++
			e.flows[f.id], e.sources[key] = f, f
		}
		if f != nil {
			f.last.Store(time.Now().UnixNano())
		}
		e.mu.Unlock()
		if f == nil {
			e.drops.Add(1)
			continue
		}
		e.write(f.id, f.target, b[:n])
	}
}

func (e *Engine) receive() {
	defer e.wg.Done()
	b := make([]byte, maxPayload)
	for {
		n, err := e.link.Read(b)
		if err != nil {
			return
		}
		if n < 10 {
			e.drops.Add(1)
			continue
		}
		id := binary.BigEndian.Uint64(b)
		size := int(binary.BigEndian.Uint16(b[8:]))
		if id == 0 || size > 255 || size > n-10 {
			e.drops.Add(1)
			continue
		}
		target := string(b[10 : 10+size])
		payload := b[10+size : n]
		if !e.allowed(len(payload)) {
			e.drops.Add(1)
			continue
		}
		e.mu.Lock()
		f := e.flows[id]
		if e.server {
			e.mu.Unlock()
			if f == nil || size != 0 {
				e.drops.Add(1)
				continue
			}
			f.last.Store(time.Now().UnixNano())
			_ = f.entry.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := f.entry.WriteToUDP(payload, f.peer); err == nil {
				e.bytesIn.Add(uint64(len(payload)))
				metrics.AddBytes(uint64(len(payload)), 0)
			} else {
				e.drops.Add(1)
			}
			continue
		}
		if f == nil && len(e.flows) < e.max && validTarget(target) {
			f = &flow{id: id, target: target, queue: make(chan []byte, 4)}
			f.last.Store(time.Now().UnixNano())
			e.flows[id] = f
			e.wg.Add(1)
			go e.backend(f)
		}
		e.mu.Unlock()
		if f == nil || target != f.target {
			e.drops.Add(1)
			continue
		}
		f.last.Store(time.Now().UnixNano())
		select {
		case f.queue <- append([]byte(nil), payload...):
			e.bytesIn.Add(uint64(len(payload)))
			metrics.AddBytes(uint64(len(payload)), 0)
		default:
			e.drops.Add(1)
		}
	}
}

func validTarget(target string) bool {
	host, port, err := net.SplitHostPort(target)
	n, pe := strconv.Atoi(port)
	return err == nil && pe == nil && host != "" && n >= 1 && n <= 65535 && len(target) <= 255
}

func (e *Engine) backend(f *flow) {
	defer e.wg.Done()
	defer func() {
		e.mu.Lock()
		if e.flows[f.id] == f {
			delete(e.flows, f.id)
		}
		e.mu.Unlock()
	}()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	dialCtx, stopDial := context.WithTimeout(ctx, 2*time.Second)
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "udp", f.target)
	stopDial()
	if err != nil {
		e.drops.Add(1)
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	reader := make(chan struct{})
	go func() {
		defer close(reader)
		defer cancel()
		b := make([]byte, 65535)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(idle))
			n, err := conn.Read(b)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() && time.Since(time.Unix(0, f.last.Load())) < idle {
					continue
				}
				return
			}
			f.last.Store(time.Now().UnixNano())
			e.write(f.id, "", b[:n])
		}
	}()
	defer func() { cancel(); conn.Close(); <-reader }()
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-f.queue:
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
			if _, err := conn.Write(payload); err != nil {
				e.drops.Add(1)
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(idle))
		}
	}
}

func (e *Engine) reap() {
	defer e.wg.Done()
	t := time.NewTicker(idle / 4)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-t.C:
			if !e.server {
				continue
			}
			e.mu.Lock()
			for id, f := range e.flows {
				if now.Sub(time.Unix(0, f.last.Load())) > idle {
					delete(e.flows, id)
					delete(e.sources, f.entry.LocalAddr().String()+"/"+f.peer.String())
				}
			}
			e.mu.Unlock()
		}
	}
}
