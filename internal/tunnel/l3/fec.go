package l3

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
)

// Forward error correction, as a carrier that wraps a carrier.
//
// A layer-3 tunnel may not sit on anything that retransmits — see the package
// doc for why stacking retransmit timers makes throughput collapse rather than
// degrade. That rules out reliability; it does not rule out REDUNDANCY. For
// every group of data packets this sends a few parity packets, so a receiver
// that lost one can rebuild it from the rest without anybody waiting for a
// timer. On a path that drops a small percentage steadily — a congested
// international route, a lossy last mile — that is the difference between a
// tunnel that stutters and one that does not.
//
// It is written as a DatagramCarrier decorator rather than as part of the spoof
// carrier, for two reasons. The MTU falls out for free: Overhead() adds this
// layer's bytes to the one below, and the tunnel's existing calculation carries
// it through. And loss is a property of the path, not of the disguise the
// packets wear, so udp, spoof, pck and xdi all get it from one implementation.
//
// # What it does not do
//
//   - It does not reorder or delay. A data packet is written to the wire the
//     moment it arrives and delivered the moment it is read; parity is extra
//     traffic behind it, never a reason to hold a packet back.
//   - It does not retransmit, ever. A group with more losses than it has parity
//     is a group with a hole, and the layer above deals with that as it always
//     has.
//   - It suppresses byte-identical copies within a group. Conflicting data
//     shards still reach the tunnel above, where authentication and replay
//     protection decide which packets to accept.

// The wire format this layer prepends to every carrier datagram:
//
//	 0        1                    5        6
//	+--------+--------------------+--------+---------------+
//	| kind   | group (uint32BE)   | index  | body          |
//	+--------+--------------------+--------+---------------+
//
// A data body is the caller's datagram behind a two-byte length; a parity body
// is one Reed-Solomon shard, which is as long as the largest shard in its group.
// The length is what lets a rebuilt shard be trimmed back to the packet it was,
// since every shard in a group is padded to a common size before the parity is
// computed over them.
// Authenticated frames set the high bit of kind and append a 16-byte MAC
// over the header and body. The tag is outside the Reed-Solomon shard.
const (
	fecHeaderLen = 6
	fecLenPrefix = 2

	fecKindData   byte = 0
	fecKindParity byte = 1
	fecKindAuth   byte = 0x80
)

// fecOverhead is what this layer costs a data packet: its header and the length
// that precedes the payload. Parity packets are extra packets rather than extra
// bytes on an existing one, so they do not enter the MTU.
const fecOverhead = fecHeaderLen + fecLenPrefix
const fecTagLen = 16

type fecKeys struct {
	send [sha256.Size]byte
	recv [sha256.Size]byte
}

func deriveFECKeys(token string, dial bool) *fecKeys {
	toListener := sha256.Sum256([]byte("backpack l3 fec v1 dial\x00" + token))
	toDialer := sha256.Sum256([]byte("backpack l3 fec v1 listen\x00" + token))
	if dial {
		return &fecKeys{send: toListener, recv: toDialer}
	}
	return &fecKeys{send: toDialer, recv: toListener}
}

func fecMAC(key *[sha256.Size]byte, frame []byte) [sha256.Size]byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write(frame)
	var tag [sha256.Size]byte
	mac.Sum(tag[:0])
	return tag
}

// fecWindow is how many recent groups a receiver keeps shards for. A group is
// only useful until its data has been delivered and its parity can no longer
// rebuild anything; holding a few is enough to cover reordering across a group
// boundary, and holding many would be memory spent on packets nobody is waiting
// for any more.
const fecWindow = 16

// Retain ordinary packet buffers across groups. Larger shards still work but
// do not pin jumbo-sized allocations in every slot after the burst ends.
const fecReuseShardCap = 2048

// FECConfig describes a scheme: for every Data packets,
// Parity extra ones are sent, and any Parity of the group's packets may be lost
// without losing anything. Both zero means no FEC at all. Auth requires
// both peers to sign and verify the wire frames.
type FECConfig struct {
	Data   int
	Parity int
	Auth   bool
}

// Enabled reports whether a scheme was asked for. A half-configured pair is not
// a scheme — parity with nothing to protect, or data with no redundancy — so it
// reads as off rather than being quietly repaired into something the other end
// did not agree to.
func (f FECConfig) Enabled() bool { return f.Data > 0 && f.Parity > 0 }

// Validate rejects a scheme the encoder cannot build, so a configuration is
// refused where it is written rather than at the first packet.
func (f FECConfig) Validate() error {
	if !f.Enabled() {
		if f.Auth {
			return fmt.Errorf("l3: fec_auth requires fec_data and fec_parity")
		}
		if f.Data != 0 || f.Parity != 0 {
			return fmt.Errorf("l3: fec needs both fec_data and fec_parity set (got %d and %d); "+
				"leave both at 0 for no error correction", f.Data, f.Parity)
		}
		return nil
	}
	if f.Data > 256-f.Parity {
		return fmt.Errorf("l3: fec_data (%d) + fec_parity (%d) exceeds the 256-shard Reed-Solomon limit",
			f.Data, f.Parity)
	}
	if f.Parity >= f.Data {
		return fmt.Errorf("l3: fec_parity (%d) is not smaller than fec_data (%d); that sends more "+
			"redundancy than payload, which costs more than the loss it repairs", f.Parity, f.Data)
	}
	return nil
}

// fecCarrier is the decorator. It owns the carrier below it and closes it.
type fecCarrier struct {
	below DatagramCarrier
	cfg   FECConfig
	enc   reedsolomon.Encoder
	keys  *fecKeys

	sendMu    sync.Mutex
	sendGrp   uint32
	sendIdx   int
	sendPad   [][]byte // padded shards of the group being built
	sendMax   int      // the longest shard so far in this group
	sendPeer  net.Addr // where the group's parity goes, from the last write
	sendFrame []byte   // reused wire frame, under sendMu

	readMu    sync.Mutex
	readFrame []byte // receive scratch, held until the payload has been copied

	recvMu  sync.Mutex
	groups  map[uint32]*fecGroup
	order   []uint32   // group ids in arrival order, for eviction
	pending []fecReady // rebuilt packets waiting to be read
}

// fecReady is a rebuilt packet and the address it is attributed to.
// Reconstructed packets authenticate their bytes, not the source of a missing shard.
type fecRecoveredAddr struct{ net.Addr }

type fecReady struct {
	data []byte
	addr net.Addr
}

// fecGroup is the receiver's state for one group.
type fecGroup struct {
	shards    [][]byte // zero length where not yet received; index by shard number
	haveData  int      // how many data shards arrived
	haveTotal int      // data + parity shards arrived
	size      int      // the padded shard size, learned from any parity shard
	done      bool     // reconstruction already attempted and satisfied
	addr      net.Addr
}

// newFECCarrier wraps below in the scheme. An unusable scheme is an error here
// rather than a silent pass-through, because a tunnel whose peer is adding
// parity and whose own end is not will discard every parity packet as garbage.
func newFECCarrier(below DatagramCarrier, cfg FECConfig, keys *fecKeys) (DatagramCarrier, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Enabled() {
		return below, nil
	}
	if cfg.Auth && keys == nil {
		return nil, fmt.Errorf("l3: authenticated FEC requires directional keys")
	}
	enc, err := reedsolomon.New(cfg.Data, cfg.Parity)
	if err != nil {
		return nil, fmt.Errorf("l3: fec %d/%d: %w", cfg.Data, cfg.Parity, err)
	}
	// The receiver keeps recent groups across peer reconnects. Restarting at
	// zero could therefore make its delivered bitmap discard a fresh handshake
	// as an old shard. Start in a fresh part of the existing wire ID space.
	var first [4]byte
	if _, err := rand.Read(first[:]); err != nil {
		return nil, fmt.Errorf("l3: choosing the initial FEC group: %w", err)
	}
	return &fecCarrier{
		sendGrp: binary.BigEndian.Uint32(first[:]),
		below:   below,
		cfg:     cfg,
		keys:    keys,
		enc:     enc,
		groups:  make(map[uint32]*fecGroup),
	}, nil
}

func (c *fecCarrier) tagLen() int {
	if c.keys != nil {
		return fecTagLen
	}
	return 0
}

func (c *fecCarrier) Overhead() int { return c.below.Overhead() + fecOverhead + c.tagLen() }

func (c *fecCarrier) CarrierName() string {
	return fmt.Sprintf("%s+fec%d/%d", c.below.CarrierName(), c.cfg.Data, c.cfg.Parity)
}

func (c *fecCarrier) Close() error                       { return c.below.Close() }
func (c *fecCarrier) LocalAddr() net.Addr                { return c.below.LocalAddr() }
func (c *fecCarrier) SetDeadline(t time.Time) error      { return c.below.SetDeadline(t) }
func (c *fecCarrier) SetReadDeadline(t time.Time) error  { return c.below.SetReadDeadline(t) }
func (c *fecCarrier) SetWriteDeadline(t time.Time) error { return c.below.SetWriteDeadline(t) }

// WriteTo sends p as a data shard immediately, and — when it completes a group —
// the group's parity behind it.
//
// The data packet is never held: the parity is computed and sent after it, so
// this layer costs bandwidth and CPU but not a millisecond of latency on the
// packet the application is waiting for.
func (c *fecCarrier) WriteTo(p []byte, addr net.Addr) (int, error) {
	// Recovered ingress metadata must not become a learned data route, but a
	// successfully authenticated init may still receive an immediate response.
	if recovered, ok := addr.(*fecRecoveredAddr); ok {
		addr = recovered.Addr
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if len(p) > 65535 {
		return 0, fmt.Errorf("l3: FEC payload of %d bytes exceeds its length field", len(p))
	}
	if c.sendPad == nil {
		c.sendPad = make([][]byte, c.cfg.Data+c.cfg.Parity)
	}
	// Each slot owns its shard until parity has been sent. Keep its capacity
	// across groups instead of allocating two copies per data packet.
	need := fecLenPrefix + len(p)
	shard := c.sendPad[c.sendIdx]
	if cap(shard) < need {
		shard = make([]byte, need)
	} else {
		shard = shard[:need]
	}
	binary.BigEndian.PutUint16(shard[:fecLenPrefix], uint16(len(p)))
	copy(shard[fecLenPrefix:], p)
	c.sendPad[c.sendIdx] = shard
	c.sendMax = max(c.sendMax, need)
	c.sendPeer = addr
	frame := c.wireFrame(fecHeaderLen + need)
	putFECHeader(frame, fecKindData, c.sendGrp, byte(c.sendIdx))
	copy(frame[fecHeaderLen:], shard)
	err := c.writeFrame(frame, addr)
	c.sendIdx++
	if c.sendIdx == c.cfg.Data {
		c.flushParity()
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// wireFrame is borrowed until the underlying synchronous write returns.
// Every caller holds sendMu, including parity generation.
func (c *fecCarrier) wireFrame(size int) []byte {
	if cap(c.sendFrame) < size+c.tagLen() {
		c.sendFrame = make([]byte, size+c.tagLen())
	}
	return c.sendFrame[:size]
}

func (c *fecCarrier) writeFrame(frame []byte, addr net.Addr) error {
	if c.keys != nil {
		frame[0] |= fecKindAuth
		tag := fecMAC(&c.keys.send, frame)
		frame = append(frame, tag[:fecTagLen]...)
	}
	n, err := c.below.WriteTo(frame, addr)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}

// flushParity sends the group's redundancy without changing the wire format.
func (c *fecCarrier) flushParity() {
	defer func() {
		c.sendGrp++
		c.sendIdx = 0
		c.sendMax = 0
	}()
	size := c.sendMax
	if size == 0 {
		return
	}
	for i, shard := range c.sendPad {
		used := len(shard)
		if cap(shard) < size {
			padded := make([]byte, size)
			copy(padded, shard)
			shard = padded
		} else {
			shard = shard[:size]
			if i < c.cfg.Data {
				// Old group bytes must not become this group's padding.
				clear(shard[used:])
			}
		}
		c.sendPad[i] = shard
	}
	if err := c.enc.Encode(c.sendPad); err != nil {
		return
	}
	frame := c.wireFrame(fecHeaderLen + size)
	for i := c.cfg.Data; i < len(c.sendPad); i++ {
		putFECHeader(frame, fecKindParity, c.sendGrp, byte(i))
		copy(frame[fecHeaderLen:], c.sendPad[i])
		// Data is already on the wire; parity remains best effort.
		_ = c.writeFrame(frame, c.sendPeer)
	}
}

// ReadFrom returns the next datagram: a rebuilt one if any is waiting, else the
// next one off the wire. Parity packets are consumed here and never surface.
func (c *fecCarrier) ReadFrom(p []byte) (int, net.Addr, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if n, addr, ok := c.takePending(p); ok {
		return n, addr, nil
	}
	need := len(p) + fecOverhead + c.tagLen()
	if cap(c.readFrame) < need {
		c.readFrame = make([]byte, need)
	}
	buf := c.readFrame[:need]
	for {
		n, addr, err := c.below.ReadFrom(buf)
		if err != nil {
			return 0, nil, err
		}
		frame := buf[:n]
		if c.keys != nil {
			if n < fecOverhead+fecTagLen || frame[0]&fecKindAuth == 0 {
				continue
			}
			frame = frame[:n-fecTagLen]
			want := fecMAC(&c.keys.recv, frame)
			if !hmac.Equal(want[:fecTagLen], buf[n-fecTagLen:n]) {
				continue
			}
			frame[0] &^= fecKindAuth
		}
		kind, group, index, body, ok := parseFECHeader(frame)
		if !ok {
			continue // not ours, or truncated: the layer above never sees it
		}
		payload, delivered := c.absorb(kind, group, index, body, addr)
		if !delivered {
			// A parity packet, or a duplicate. It may have completed a group,
			// so check the rebuilt queue before going back to the wire.
			if n, a, ok := c.takePending(p); ok {
				return n, a, nil
			}
			continue
		}
		return copy(p, payload), addr, nil
	}
}

// takePending pops one rebuilt packet, if there is one.
func (c *fecCarrier) takePending(p []byte) (int, net.Addr, bool) {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()
	if len(c.pending) == 0 {
		return 0, nil, false
	}
	r := c.pending[0]
	c.pending[0] = fecReady{}
	c.pending = c.pending[1:]
	return copy(p, r.data), r.addr, true
}

// absorb files a shard and, when a group can be completed, queues whatever it
// rebuilds. It returns the payload to deliver now for a data shard.
func (c *fecCarrier) absorb(kind byte, group uint32, index byte, body []byte, addr net.Addr) ([]byte, bool) {
	c.recvMu.Lock()
	defer c.recvMu.Unlock()

	total := c.cfg.Data + c.cfg.Parity
	if int(index) >= total || (kind == fecKindData && int(index) >= c.cfg.Data) ||
		(kind == fecKindParity && int(index) < c.cfg.Data) ||
		(kind != fecKindData && kind != fecKindParity) {
		return nil, false
	}
	// Reject malformed lengths before they can occupy a shard or mark it delivered.
	// This is structural validation; the outer FEC format is not authenticated.
	var payload []byte
	if kind == fecKindData {
		var ok bool
		payload, ok = trimShard(body)
		if !ok {
			return nil, false
		}
	}
	if len(body) < fecLenPrefix {
		return nil, false
	}
	g := c.groups[group]
	if g == nil {
		g = c.newGroup(group, total, addr)
	}

	if kind == fecKindParity && (g.done || g.haveData == c.cfg.Data) {
		return nil, false
	}
	if existing := g.shards[index]; len(existing) != 0 {
		if kind == fecKindData {
			prior, _ := trimShard(existing)
			// An unauthenticated shard can claim the same slot as a real
			// packet. Only identical payloads can be discarded here.
			if !bytes.Equal(prior, payload) {
				return payload, true
			}
		}
		return nil, false
	}
	g.shards[index] = append(g.shards[index][:0], body...)
	g.haveTotal++
	if kind == fecKindData {
		g.haveData++
	} else if g.size == 0 {
		// Every parity shard is full size.
		g.size = len(body)
	}
	c.rebuild(g)
	return payload, kind == fecKindData
}

// rebuild fills a group's holes from its parity, when there is enough to do it,
// and queues the data packets that were missing. The caller holds recvMu.
func (c *fecCarrier) rebuild(g *fecGroup) {
	if g.done || g.haveData == c.cfg.Data || g.haveTotal < c.cfg.Data || g.size == 0 {
		return
	}
	// Reed-Solomon needs every present shard at the common width; the data
	// shards travelled unpadded, so they are widened back here.
	work := make([][]byte, len(g.shards))
	for i, s := range g.shards {
		if len(s) == 0 {
			continue
		}
		if len(s) == g.size {
			work[i] = s
			continue
		}
		if len(s) > g.size {
			return // a shard wider than the group: not a group we can rebuild
		}
		padded := make([]byte, g.size)
		copy(padded, s)
		work[i] = padded
	}
	if err := c.enc.Reconstruct(work); err != nil {
		return
	}
	g.done = true
	for i := 0; i < c.cfg.Data; i++ {
		if len(g.shards[i]) != 0 || work[i] == nil {
			continue
		}
		if payload, ok := trimShard(work[i]); ok {
			g.shards[i] = work[i]
			c.pending = append(c.pending, fecReady{data: payload, addr: &fecRecoveredAddr{g.addr}})
		}
	}
}

// newGroup replaces the oldest group when the receive window is full. The
// caller holds recvMu. A rebuilt packet may still be owned by pending, so
// reconstructed groups never lend their storage to a new group.
func (c *fecCarrier) newGroup(id uint32, total int, addr net.Addr) *fecGroup {
	var g *fecGroup
	if len(c.order) == fecWindow {
		oldest := c.order[0]
		g = c.groups[oldest]
		delete(c.groups, oldest)
		copy(c.order, c.order[1:])
		c.order[len(c.order)-1] = id
		for i, shard := range g.shards {
			if g.done || cap(shard) > fecReuseShardCap {
				g.shards[i] = nil
			} else {
				g.shards[i] = shard[:0]
			}
		}
		*g = fecGroup{shards: g.shards, addr: addr}
	} else {
		g = &fecGroup{shards: make([][]byte, total), addr: addr}
		c.order = append(c.order, id)
	}
	c.groups[id] = g
	return g
}

// putFECHeader writes the fixed prefix.
func putFECHeader(dst []byte, kind byte, group uint32, index byte) {
	_ = dst[fecHeaderLen-1]
	dst[0] = kind
	binary.BigEndian.PutUint32(dst[1:5], group)
	dst[5] = index
}

// parseFECHeader splits a received datagram. ok is false for anything too short
// or carrying a kind this layer does not write, which is how a packet from
// something that is not this tunnel is dropped before it can confuse a group.
func parseFECHeader(buf []byte) (kind byte, group uint32, index byte, body []byte, ok bool) {
	if len(buf) < fecHeaderLen {
		return 0, 0, 0, nil, false
	}
	kind = buf[0]
	if kind != fecKindData && kind != fecKindParity {
		return 0, 0, 0, nil, false
	}
	return kind, binary.BigEndian.Uint32(buf[1:5]), buf[5], buf[fecHeaderLen:], true
}

// trimShard reads the length a shard was padded from and returns the packet.
// ok is false for a shard whose length does not fit inside it, which is what a
// corrupt or mis-sized rebuild looks like.
func trimShard(shard []byte) ([]byte, bool) {
	if len(shard) < fecLenPrefix {
		return nil, false
	}
	n := int(binary.BigEndian.Uint16(shard[:fecLenPrefix]))
	if n > len(shard)-fecLenPrefix {
		return nil, false
	}
	return shard[fecLenPrefix : fecLenPrefix+n], true
}
