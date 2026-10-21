// Package nativeudp carries reverse UDP flows as authenticated, unordered
// messages through Hysteria2/TUIC's native UDP relay. It never retransmits user
// datagrams. Noise NNpsk0 supplies fresh direction-specific keys; WireGuard's
// replay filter is committed only after successful AEAD authentication.
package nativeudp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flynn/noise"
	"golang.zx2c4.com/wireguard/replay"
)

const headerSize = 25
const maxPayload = 60000
const sessionLife = 5 * time.Minute
const rekeyAfter = 2 * time.Minute

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
var errNotReady = errors.New("native UDP peer is not authenticated")

type keys struct {
	id         [16]byte
	stamp      uint64
	created    time.Time
	send, recv noise.Cipher
	counter    uint64
	replay     replay.Filter
	response   []byte
}

type attempt struct {
	id                [16]byte
	stamp             uint64
	state             *noise.HandshakeState
	packet            []byte
	started, lastSent time.Time
}

type Stats struct {
	Connected bool   `json:"connected"`
	Sent      uint64 `json:"sent"`
	Received  uint64 `json:"received"`
	Dropped   uint64 `json:"dropped"`
}

type Link struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	pc                      net.PacketConn
	dial                    bool
	psk                     [32]byte
	probeKey                [32]byte
	mu                      sync.Mutex
	peer                    net.Addr
	active                  *keys
	previous                *keys
	pending                 map[[16]byte]*keys
	assemblies              map[fragmentKey]*assembly
	messageSeq              atomic.Uint64
	attempt                 *attempt
	highStamp               uint64
	lastStamp               uint64
	lastRx                  time.Time
	wmu                     sync.Mutex
	wbuf                    []byte
	plain                   []byte // framing scratch, guarded by wmu like wbuf
	in                      chan []byte
	wg                      sync.WaitGroup
	once                    sync.Once
	sent, received, dropped atomic.Uint64
}

func NewLink(parent context.Context, pc net.PacketConn, peer net.Addr, token string) (*Link, error) {
	if len(token) < 16 {
		return nil, errors.New("native UDP requires a strong reverse token")
	}
	ctx, cancel := context.WithCancel(parent)
	l := &Link{ctx: ctx, cancel: cancel, pc: pc, peer: peer, dial: peer != nil, psk: sha256.Sum256([]byte("backpack reverse native UDP psk v1\x00" + token)), pending: map[[16]byte]*keys{}, in: make(chan []byte, 128)}
	l.assemblies = make(map[fragmentKey]*assembly)
	l.wg.Add(2)
	l.probeKey = sha256.Sum256([]byte("backpack native UDP path proof v1\x00" + token))
	go l.read()
	go l.maintain()
	return l, nil
}

func (l *Link) Stats() Stats {
	l.mu.Lock()
	up := l.active != nil && time.Since(l.active.created) < sessionLife && time.Since(l.lastRx) < 15*time.Second
	l.mu.Unlock()
	return Stats{up, l.sent.Load(), l.received.Load(), l.dropped.Load()}
}

func (l *Link) Close() error { l.once.Do(func() { l.cancel(); l.pc.Close(); l.wg.Wait() }); return nil }

func (l *Link) handshake(initiator bool, id [16]byte) (*noise.HandshakeState, error) {
	return noise.NewHandshakeState(noise.Config{CipherSuite: suite, Pattern: noise.HandshakeNN, Initiator: initiator, Prologue: append([]byte("backpack reverse native UDP v1\x00"), id[:]...), PresharedKey: l.psk[:], PresharedKeyPlacement: 0})
}

func envelope(kind byte, id [16]byte, counter uint64) []byte {
	b := make([]byte, headerSize)
	b[0] = kind
	copy(b[1:17], id[:])
	binary.BigEndian.PutUint64(b[17:], counter)
	return b
}

func (l *Link) begin(now time.Time) error {
	a := &attempt{started: now}
	if _, err := rand.Read(a.id[:]); err != nil {
		return err
	}
	a.stamp = uint64(now.UnixNano())
	if a.stamp <= l.lastStamp {
		a.stamp = l.lastStamp + 1
	}
	l.lastStamp = a.stamp
	hs, err := l.handshake(true, a.id)
	if err != nil {
		return err
	}
	a.state = hs
	payload := binary.BigEndian.AppendUint64(nil, a.stamp)
	msg, _, _, err := hs.WriteMessage(nil, payload)
	if err != nil {
		return err
	}
	a.packet = append(envelope(1, a.id, 0), msg...)
	l.attempt = a
	return nil
}

func (l *Link) maintain() {
	defer l.wg.Done()
	stop := context.AfterFunc(l.ctx, func() { l.pc.Close() })
	defer stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		now := time.Now()
		l.mu.Lock()
		for id, a := range l.assemblies {
			if now.Sub(a.created) > assemblyLife {
				delete(l.assemblies, id)
				l.dropped.Add(1)
			}
		}
		for id, k := range l.pending {
			if now.Sub(k.created) > 5*time.Second {
				delete(l.pending, id)
			}
		}
		if l.previous != nil && now.Sub(l.previous.created) > sessionLife {
			l.previous = nil
		}
		need := l.active == nil || now.Sub(l.active.created) > rekeyAfter || (!l.lastRx.IsZero() && now.Sub(l.lastRx) > 10*time.Second)
		var packet []byte
		peer := l.peer
		if l.dial && need {
			if l.attempt == nil || now.Sub(l.attempt.started) > 5*time.Second {
				_ = l.begin(now)
			}
			if l.attempt != nil && now.Sub(l.attempt.lastSent) >= 500*time.Millisecond {
				packet = append([]byte(nil), l.attempt.packet...)
				l.attempt.lastSent = now
			}
		}
		active := l.active != nil
		l.mu.Unlock()
		if packet != nil && peer != nil {
			_ = l.rawWrite(packet, peer)
		}
		if active {
			_ = l.send([]byte{0})
		}
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (l *Link) rawWrite(b []byte, peer net.Addr) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if l.ctx.Err() != nil {
		return net.ErrClosed
	}
	_ = l.pc.SetWriteDeadline(time.Now().Add(time.Second))
	n, err := l.pc.WriteTo(b, peer)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func (l *Link) send(payload []byte) error {
	return l.sendParts(nil, payload)
}

// sendParts serializes reusable plaintext and wire buffers through the socket
// write. The packet connection consumes them before another sender reuses them.
func (l *Link) sendParts(prefix, payload []byte) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	k, peer := l.sendKey()
	return l.sendPartsLocked(k, peer, prefix, payload)
}

func (l *Link) sendKey() (*keys, net.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active, l.peer
}

// The writer lock covers nonce assignment and the socket write, so local
// senders cannot push an earlier counter behind arbitrarily many later ones.
// A fragmented message supplies the same key/peer snapshot for all its parts.
func (l *Link) sendPartsLocked(k *keys, peer net.Addr, prefix, payload []byte) error {
	l.mu.Lock()
	if k == nil || peer == nil || time.Since(k.created) > sessionLife || k.counter >= 1<<60 {
		l.mu.Unlock()
		return errNotReady
	}
	counter := k.counter
	k.counter++
	// Noise's primitive Cipher is safe with explicit independent nonces. The
	// shared scratch and socket are serialized without holding the receive lock.
	id := k.id
	cipher := k.send
	l.mu.Unlock()
	if l.ctx.Err() != nil {
		return net.ErrClosed
	}
	if cap(l.wbuf) < headerSize {
		l.wbuf = make([]byte, headerSize, headerSize+fragmentHeader+fragmentSize+16)
	}
	l.wbuf = l.wbuf[:headerSize]
	l.wbuf[0] = 3
	copy(l.wbuf[1:17], id[:])
	binary.BigEndian.PutUint64(l.wbuf[17:], counter)
	if len(prefix) != 0 {
		l.plain = append(l.plain[:0], prefix...)
		l.plain = append(l.plain, payload...)
		payload = l.plain
	}
	l.wbuf = cipher.Encrypt(l.wbuf, counter, l.wbuf[:headerSize], payload)
	_ = l.pc.SetWriteDeadline(time.Now().Add(time.Second))
	n, err := l.pc.WriteTo(l.wbuf, peer)
	if err == nil && n != len(l.wbuf) {
		err = io.ErrShortWrite
	}
	return err
}

func (l *Link) Write(b []byte) error {
	if len(b) > maxPayload {
		return errors.New("native UDP payload too large")
	}
	if err := l.sendMessage(b); err != nil {
		l.dropped.Add(1)
		return err
	}
	l.sent.Add(1)
	return nil
}

func (l *Link) Read(b []byte) (int, error) {
	select {
	case p := <-l.in:
		if len(p) > len(b) {
			return 0, io.ErrShortBuffer
		}
		return copy(b, p), nil
	case <-l.ctx.Done():
		return 0, net.ErrClosed
	}
}

func (l *Link) read() {
	defer l.wg.Done()
	b := make([]byte, 65535)
	for {
		n, from, err := l.pc.ReadFrom(b)
		if err != nil {
			if l.ctx.Err() == nil {
				l.cancel()
			}
			return
		}
		if n < headerSize || n > maxPayload+headerSize+17 {
			l.dropped.Add(1)
			continue
		}
		var id [16]byte
		if b[0] == 4 && !l.dial {
			l.answerProbe(b[:n], from)
			continue
		}
		copy(id[:], b[1:17])
		if l.dial && from.String() != l.peer.String() {
			l.dropped.Add(1)
			continue
		}
		var reply []byte
		var message []byte
		var pong bool
		l.mu.Lock()
		switch b[0] {
		case 1:
			if !l.dial && binary.BigEndian.Uint64(b[17:25]) == 0 {
				if k := l.pending[id]; k != nil {
					reply = append([]byte(nil), k.response...)
				} else if len(l.pending) < 8 {
					hs, e := l.handshake(false, id)
					if e == nil {
						payload, _, _, e := hs.ReadMessage(nil, b[headerSize:n])
						if e == nil && len(payload) == 8 {
							stamp := binary.BigEndian.Uint64(payload)
							if stamp > l.highStamp {
								msg, c1, c2, e := hs.WriteMessage(nil, nil)
								if e == nil && c1 != nil && c2 != nil {
									reply = append(envelope(2, id, 0), msg...)
									l.pending[id] = &keys{id: id, stamp: stamp, created: time.Now(), send: c2.Cipher(), recv: c1.Cipher(), response: reply}
								}
							}
						}
					}
				}
			}
		case 2:
			if l.dial && l.attempt != nil && id == l.attempt.id && binary.BigEndian.Uint64(b[17:25]) == 0 {
				a := l.attempt
				_, c1, c2, e := a.state.ReadMessage(nil, b[headerSize:n])
				if e == nil && c1 != nil && c2 != nil {
					l.previous = l.active
					l.active = &keys{id: id, stamp: a.stamp, created: time.Now(), send: c1.Cipher(), recv: c2.Cipher()}
					l.attempt = nil
					l.lastRx = time.Now()
					pong = true
				}
			}
		case 3:
			var k *keys
			if l.active != nil && l.active.id == id {
				k = l.active
			} else if l.previous != nil && l.previous.id == id {
				k = l.previous
			} else if !l.dial {
				k = l.pending[id]
			}
			if k != nil && time.Since(k.created) < sessionLife {
				counter := binary.BigEndian.Uint64(b[17:25])
				payload, e := k.recv.Decrypt(nil, counter, b[:headerSize], b[headerSize:n])
				if e == nil && k.replay.ValidateCounter(counter, 1<<60) && len(payload) > 0 {
					if !l.dial && l.pending[id] == k {
						delete(l.pending, id)
						if k.stamp <= l.highStamp {
							break
						}
						l.highStamp = k.stamp
						l.previous = l.active
						l.active = k
					}
					if !l.dial {
						l.peer = from
					}
					l.lastRx = time.Now()
					switch payload[0] {
					case 0:
						pong = true
					case 1:
					case 2:
						message = payload[1:]
					case 3:
						message = l.assemble(id, payload)
					}
				}
			}
		}
		l.mu.Unlock()
		if reply != nil {
			_ = l.rawWrite(reply, from)
		}
		if pong {
			_ = l.send([]byte{1})
		}
		if message != nil {
			select {
			case l.in <- message:
				l.received.Add(1)
			default:
				l.dropped.Add(1)
			}
		}
	}
}
