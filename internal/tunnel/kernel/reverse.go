package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/utils/network"
)

const reverseLanes = 8

type packetDevice interface{ io.ReadWriteCloser }
type reversePool struct {
	ctx      context.Context
	mu       sync.RWMutex
	lanes    [reverseLanes]*reverseLane
	device   packetDevice
	identity [32]byte
}
type reversePacket struct {
	data [16384]byte
	n    int
}

var reversePackets = sync.Pool{New: func() any { return new(reversePacket) }}

type reverseLane struct {
	conn    net.Conn
	queue   chan *reversePacket
	done    chan struct{}
	once    sync.Once
	queueMu sync.Mutex
}

func (l *reverseLane) close() {
	l.once.Do(func() { l.queueMu.Lock(); close(l.done); l.queueMu.Unlock(); _ = l.conn.Close() })
}
func (l *reverseLane) enqueue(packet *reversePacket) bool {
	l.queueMu.Lock()
	defer l.queueMu.Unlock()
	select {
	case <-l.done:
		return false
	default:
	}
	select {
	case l.queue <- packet:
		return true
	default:
		return false
	}
}

func reverse(ctx context.Context, c config.KernelConfig, ready func()) error {
	dev, err := openDevice(c.Interface+"r", false)
	if err != nil {
		return err
	}
	defer dev.Close()
	return runReverseReady(ctx, c, dev, ready)
}

func runReverse(parent context.Context, c config.KernelConfig, dev packetDevice) error {
	return runReverseReady(parent, c, dev, nil)
}

func runReverseReady(parent context.Context, c config.KernelConfig, dev packetDevice, ready func()) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	p := &reversePool{ctx: ctx, device: dev, identity: sha256.Sum256([]byte(fmt.Sprintf("backpack-kernel-reverse-v1|%s|%d|%d|%s", c.Method, c.ID, c.MTU, c.Token)))}
	stop := context.AfterFunc(ctx, func() {
		_ = dev.Close()
		p.mu.RLock()
		defer p.mu.RUnlock()
		for _, lane := range p.lanes {
			if lane != nil {
				lane.close()
			}
		}
	})
	defer stop()
	var wg sync.WaitGroup
	defer func() {
		cancel()
		_ = dev.Close()
		p.mu.RLock()
		for _, lane := range p.lanes {
			if lane != nil {
				lane.close()
			}
		}
		p.mu.RUnlock()
		wg.Wait()
	}()
	failed := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 16384)
		for {
			n, e := dev.Read(buf)
			if e != nil {
				select {
				case failed <- e:
				default:
				}
				return
			}
			laneIndex := int(flowHash(buf[:n]) % reverseLanes)
			p.mu.RLock()
			lane := p.lanes[laneIndex]
			if lane != nil {
				packet := reversePackets.Get().(*reversePacket)
				packet.n = copy(packet.data[:], buf[:n])
				if !lane.enqueue(packet) {
					reversePackets.Put(packet)
				}
			}
			p.mu.RUnlock()
		}
	}()
	if c.Role == "iran" {
		ln, e := net.Listen("tcp", net.JoinHostPort(c.LocalAddr, number(c.Port)))
		if e != nil {
			return e
		}
		defer ln.Close()
		stopListener := context.AfterFunc(ctx, func() { _ = ln.Close() })
		defer stopListener()
		// Admission includes unauthenticated handshakes, bounding memory and
		// goroutines even if an exposed carrier port is continuously probed.
		slots := make(chan struct{}, 16)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				raw, e := ln.Accept()
				if e != nil {
					select {
					case failed <- e:
					default:
					}
					return
				}
				select {
				case slots <- struct{}{}:
					wg.Add(1)
					go func() { defer wg.Done(); defer func() { <-slots }(); p.connection(raw, c.Token, -1) }()
				default:
					_ = raw.Close()
				}
			}
		}()
	} else {
		for i := 0; i < reverseLanes; i++ {
			wg.Add(1)
			go func(index int) {
				defer wg.Done()
				for ctx.Err() == nil {
					raw, e := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(c.RemoteAddr, number(c.Port)))
					if e == nil {
						p.connection(raw, c.Token, index)
					}
					timer := time.NewTimer(time.Second)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}(i)
		}
	}
	if ready != nil {
		ready()
	}
	select {
	case <-parent.Done():
		return nil
	case e := <-failed:
		if parent.Err() != nil {
			return nil
		}
		return e
	}
}

func (p *reversePool) connection(raw net.Conn, token string, index int) {
	defer raw.Close()
	stop := context.AfterFunc(p.ctx, func() { _ = raw.Close() })
	defer stop()
	var conn net.Conn
	var err error
	// Domain separation prevents another Backpack Noise service using the
	// same secret from being mistaken for a kernel carrier.
	token = "backpack-kernel-reverse-v1|" + token
	if index < 0 {
		conn, err = network.NoiseServerConn(raw, token, 5*time.Second)
	} else {
		conn, err = network.NoiseClientConn(raw, token, 5*time.Second)
	}
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	hello := make([]byte, 33)
	copy(hello, p.identity[:])
	if index >= 0 {
		hello[32] = byte(index)
		if _, err = conn.Write(hello); err != nil {
			return
		}
		var ack [1]byte
		if _, err = io.ReadFull(conn, ack[:]); err != nil || ack[0] != 1 {
			return
		}
	} else {
		if _, err = io.ReadFull(conn, hello); err != nil || string(hello[:32]) != string(p.identity[:]) || int(hello[32]) >= reverseLanes {
			return
		}
		index = int(hello[32])
		if _, err = conn.Write([]byte{1}); err != nil {
			return
		}
	}
	_ = conn.SetDeadline(time.Time{})
	lane := &reverseLane{conn: conn, queue: make(chan *reversePacket, 128), done: make(chan struct{})}
	p.mu.Lock()
	prior := p.lanes[index]
	p.lanes[index] = lane
	p.mu.Unlock()
	if prior != nil {
		prior.close()
	}
	defer func() {
		lane.close()
		p.mu.Lock()
		if p.lanes[index] == lane {
			p.lanes[index] = nil
		}
		p.mu.Unlock()
	}()
	written := make(chan struct{})
	go func() {
		defer close(written)
		defer lane.close()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		frame := make([]byte, 16386)
		defer func() {
			lane.close()
			for {
				select {
				case packet := <-lane.queue:
					reversePackets.Put(packet)
				default:
					return
				}
			}
		}()
		for {
			var packet *reversePacket
			select {
			case <-p.ctx.Done():
				return
			case <-lane.done:
				return
			case packet = <-lane.queue:
			case <-tick.C:
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			n := 0
			if packet != nil {
				n = packet.n
				copy(frame[2:], packet.data[:n])
				reversePackets.Put(packet)
			}
			binary.BigEndian.PutUint16(frame[:2], uint16(n))
			if _, e := conn.Write(frame[:n+2]); e != nil {
				return
			}
		}
	}()
	defer func() { lane.close(); <-written }()
	buf := make([]byte, 16384)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		var header [2]byte
		if _, err = io.ReadFull(conn, header[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(header[:]))
		if n > len(buf) {
			return
		}
		if n == 0 {
			continue
		}
		if _, err = io.ReadFull(conn, buf[:n]); err != nil {
			return
		}
		if _, err = p.device.Write(buf[:n]); err != nil {
			return
		}
	}
}

// Hash the innermost flow, not its changing TCP sequence or packet payload.
// Packets from one flow always use the same lane; a failed lane is not silently
// replaced by another lane, which would reorder an active TCP connection.
func flowHash(packet []byte) uint32 {
	p := packet
	for depth := 0; depth < 5; depth++ {
		var proto byte
		var offset int
		var addresses []byte
		fragment := false
		if len(p) >= 20 && p[0]>>4 == 4 {
			offset = int(p[0]&15) * 4
			if offset < 20 || offset > len(p) {
				return 0
			}
			proto = p[9]
			addresses = p[12:20]
			fragment = binary.BigEndian.Uint16(p[6:8])&0x3fff != 0
		} else if len(p) >= 40 && p[0]>>4 == 6 {
			proto = p[6]
			offset = 40
			addresses = p[8:40]
		} else {
			return hashBytes(p[:min(len(p), 12)])
		}
		body := p[offset:]
		if !fragment {
			switch proto {
			case 4, 41:
				p = body
				continue
			case 47:
				if len(body) >= 4 {
					flags := binary.BigEndian.Uint16(body[:2])
					n := 4
					if flags&0x8000 != 0 {
						n += 4
					}
					if flags&0x2000 != 0 {
						n += 4
					}
					if flags&0x1000 != 0 {
						n += 4
					}
					if n <= len(body) {
						kind := binary.BigEndian.Uint16(body[2:4])
						if kind == 0x0800 || kind == 0x86dd {
							p = body[n:]
							continue
						}
						if kind == 0x6558 && len(body) >= n+14 {
							p = body[n+14:]
							continue
						}
					}
				}
			case 17:
				if len(body) >= 16 {
					port := binary.BigEndian.Uint16(body[2:4])
					if port == 4789 && len(body) >= 30 {
						p = body[30:]
						continue
					}
					if port == 6081 {
						n := 16 + int(body[8]&0x3f)*4
						if len(body) >= n+14 {
							p = body[n+14:]
							continue
						}
					}
				}
			}
		}
		h := hashBytes(addresses)
		h = (h ^ uint32(proto)) * 16777619
		if !fragment && (proto == 6 || proto == 17) && len(body) >= 4 {
			for _, v := range body[:4] {
				h = (h ^ uint32(v)) * 16777619
			}
		}
		return h
	}
	return 0
}
func hashBytes(p []byte) uint32 {
	h := uint32(2166136261)
	for _, b := range p {
		h = (h ^ uint32(b)) * 16777619
	}
	return h
}
