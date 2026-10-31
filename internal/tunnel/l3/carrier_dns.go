package l3

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const CarrierDNS = "dns"

type dnsPacket struct {
	buf  [dnsPacketLimit]byte
	n    int
	from net.Addr
}

var dnsPackets = sync.Pool{New: func() any { return new(dnsPacket) }}

// Both endpoints receive on the configured DNS ports, but send DNS queries
// from ephemeral ports. Replies therefore continue to target the peer's DNS
// port even when the peer rotates its sending socket after every few frames.
// The upper Noise engine alone decides whether an address change is trusted.
type dnsCarrier struct {
	ctx                         context.Context
	cancel                      context.CancelFunc
	closed                      sync.Once
	receivers                   []*net.UDPConn
	packets                     chan *dnsPacket
	workers                     sync.WaitGroup
	marker                      [8]byte
	basePort, paths, rotate     int
	limiter                     *rate.Limiter
	overhead                    int
	writeMu                     sync.Mutex
	scratch                     []byte
	sequence                    uint32
	sent                        int
	stateMu                     sync.Mutex
	sender                      *net.UDPConn
	bindIP                      net.IP
	readDeadline, writeDeadline time.Time
	deadlineChanged             chan struct{}
	writeChanged                chan struct{}
}

func validateDNS(c Config) error {
	if c.Carrier != CarrierDNS {
		if c.DNS.DNSBind != "" || c.DNS.DNSRotate != 0 || c.DNS.DNSRate != 0 {
			return fmt.Errorf("l3: dns_* settings require carrier=dns")
		}
		return nil
	}
	if c.DNS.DNSRotate < 0 || c.DNS.DNSRotate > 1<<20 || c.DNS.DNSRate < 0 || c.DNS.DNSRate > 1000000 {
		return fmt.Errorf("l3: dns_rotate must be 0..1048576 and dns_rate 0..1000000 packets/s")
	}
	if c.DNS.DNSBind != "" && net.ParseIP(c.DNS.DNSBind) == nil {
		return fmt.Errorf("l3: dns_bind must be a literal local IP")
	}
	_, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p+max(1, c.Multipath.Paths)-1 > 65535 {
		return fmt.Errorf("l3: DNS carrier ports must be 1..65535")
	}
	return nil
}

func openDNS(parent context.Context, cfg Config) (DatagramCarrier, net.Addr, error) {
	resolve, stop := context.WithTimeout(parent, carrierResolveTimeout)
	endpoint, _, err := resolveDatagramEndpoint(resolve, cfg.Addr)
	stop()
	if err != nil {
		return nil, nil, err
	}
	if err = cfg.Multipath.Validate(); err != nil {
		return nil, nil, err
	}
	if err = validateDNS(cfg); err != nil {
		return nil, nil, err
	}
	bindIP := endpoint.IP
	if cfg.Mode == ModeDial {
		bindIP = net.IPv4zero
		if endpoint.IP.To4() == nil {
			bindIP = net.IPv6zero
		}
	}
	if cfg.DNS.DNSBind != "" {
		bindIP = net.ParseIP(cfg.DNS.DNSBind)
		if (bindIP.To4() == nil) != (endpoint.IP.To4() == nil) {
			return nil, nil, fmt.Errorf("l3: dns_bind and addr must use the same IP family")
		}
		if cfg.Mode == ModeListen && !endpoint.IP.IsUnspecified() && !endpoint.IP.Equal(bindIP) {
			return nil, nil, fmt.Errorf("l3: dns_bind conflicts with the DNS listener address")
		}
	}
	ctx, cancel := context.WithCancel(parent)
	c := &dnsCarrier{ctx: ctx, cancel: cancel, marker: dnsMarker(cfg.Token), basePort: endpoint.Port, paths: max(1, cfg.Multipath.Paths), rotate: cfg.DNS.DNSRotate, bindIP: bindIP, packets: make(chan *dnsPacket, 256), deadlineChanged: make(chan struct{}), writeChanged: make(chan struct{}), scratch: make([]byte, dnsPacketLimit), overhead: udpOverhead(endpoint) + dnsFrameHeader}
	if cfg.DNS.DNSRate > 0 {
		c.limiter = rate.NewLimiter(rate.Limit(cfg.DNS.DNSRate), min(64, cfg.DNS.DNSRate))
	}
	var seed [4]byte
	if _, err = rand.Read(seed[:]); err != nil {
		cancel()
		return nil, nil, err
	}
	c.sequence = binary.BigEndian.Uint32(seed[:])
	for i := 0; i < c.paths; i++ {
		socket, e := net.ListenUDP("udp", &net.UDPAddr{IP: bindIP, Port: endpoint.Port + i})
		if e != nil {
			c.Close()
			return nil, nil, fmt.Errorf("l3: DNS receive port %d is unavailable: %w", endpoint.Port+i, e)
		}
		sizeUDPBuffers(socket, cfg.SockBuf)
		c.receivers = append(c.receivers, socket)
	}
	for _, socket := range c.receivers {
		c.workers.Add(1)
		go c.receive(socket)
	}
	if cfg.Mode == ModeDial {
		return c, endpoint, nil
	}
	return c, nil, nil
}

func (c *dnsCarrier) receive(socket *net.UDPConn) {
	defer c.workers.Done()
	for {
		packet := dnsPackets.Get().(*dnsPacket)
		n, from, err := socket.ReadFromUDP(packet.buf[:])
		if err != nil {
			dnsPackets.Put(packet)
			c.cancel()
			return
		}
		payload, ok := readDNSFrame(packet.buf[:n], c.marker)
		if !ok {
			dnsPackets.Put(packet)
			continue
		}
		packet.n = copy(packet.buf[:], payload)
		packet.from = &net.UDPAddr{IP: from.IP, Zone: from.Zone, Port: c.basePort}
		select {
		case c.packets <- packet:
		case <-c.ctx.Done():
			dnsPackets.Put(packet)
			return
		default:
			dnsPackets.Put(packet)
		}
	}
}

func (c *dnsCarrier) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		c.stateMu.Lock()
		deadline, changed := c.readDeadline, c.deadlineChanged
		c.stateMu.Unlock()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		select {
		case <-c.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
			continue
		case <-timeout:
			return 0, nil, os.ErrDeadlineExceeded
		case packet := <-c.packets:
			if timer != nil {
				timer.Stop()
			}
			n, from := copy(p, packet.buf[:packet.n]), packet.from
			short := n < packet.n
			packet.from = nil
			dnsPackets.Put(packet)
			if short {
				return n, from, io.ErrShortBuffer
			}
			return n, from, nil
		}
	}
}

func (c *dnsCarrier) WriteTo(p []byte, to net.Addr) (int, error) {
	if len(p) > dnsPacketLimit-dnsFrameHeader {
		return 0, fmt.Errorf("l3: DNS datagram exceeds the carrier limit")
	}
	peer, ok := to.(*net.UDPAddr)
	if !ok || peer == nil || peer.IP == nil {
		return 0, fmt.Errorf("l3: DNS carrier needs a UDP peer address")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.waitToSend(); err != nil {
		return 0, err
	}
	c.stateMu.Lock()
	if c.ctx.Err() != nil {
		c.stateMu.Unlock()
		return 0, net.ErrClosed
	}
	if c.sender == nil || c.rotate > 0 && c.sent >= c.rotate {
		fresh, err := net.ListenUDP("udp", &net.UDPAddr{IP: c.bindIP})
		if err != nil {
			c.stateMu.Unlock()
			return 0, err
		}
		sizeUDPBuffers(fresh, defaultSockBuf)
		prior := c.sender
		c.sender = fresh
		c.sent = 0
		if prior != nil {
			_ = prior.Close()
		}
	}
	socket := c.sender
	deadline := c.writeDeadline
	if deadline.IsZero() {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = socket.SetWriteDeadline(deadline)
	c.stateMu.Unlock()
	c.sequence++
	frame := putDNSFrame(c.scratch, c.marker, c.sequence, p)
	destination := &net.UDPAddr{IP: peer.IP, Zone: peer.Zone, Port: c.basePort + int(c.sequence%uint32(c.paths))}
	if _, err := socket.WriteToUDP(frame, destination); err != nil {
		return 0, err
	}
	c.sent++
	return len(p), nil
}

func (c *dnsCarrier) Close() error {
	c.closed.Do(func() {
		c.cancel()
		for _, socket := range c.receivers {
			_ = socket.Close()
		}
		c.stateMu.Lock()
		if c.sender != nil {
			_ = c.sender.Close()
		}
		c.stateMu.Unlock()
		c.workers.Wait()
		for {
			select {
			case packet := <-c.packets:
				packet.from = nil
				dnsPackets.Put(packet)
			default:
				return
			}
		}
	})
	return nil
}
func (c *dnsCarrier) LocalAddr() net.Addr {
	if len(c.receivers) == 0 {
		return &net.UDPAddr{IP: c.bindIP, Port: c.basePort}
	}
	return c.receivers[0].LocalAddr()
}
func (c *dnsCarrier) Overhead() int       { return c.overhead }
func (c *dnsCarrier) CarrierName() string { return CarrierDNS }
func (c *dnsCarrier) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}
func (c *dnsCarrier) SetReadDeadline(t time.Time) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.readDeadline = t
	close(c.deadlineChanged)
	c.deadlineChanged = make(chan struct{})
	return nil
}
func (c *dnsCarrier) SetWriteDeadline(t time.Time) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.writeDeadline = t
	if c.writeChanged != nil {
		close(c.writeChanged)
	}
	c.writeChanged = make(chan struct{})
	if c.sender != nil {
		return c.sender.SetWriteDeadline(t)
	}
	return nil
}

// Reservations are cancellable and deadlines can change while pacing waits.
// Close wakes these waits before taking the sender state lock.
func (c *dnsCarrier) waitToSend() error {
	readyAt := time.Now()
	var reservation *rate.Reservation
	if c.limiter != nil {
		reservation = c.limiter.Reserve()
		readyAt = readyAt.Add(reservation.Delay())
		defer func() {
			if time.Now().Before(readyAt) {
				reservation.Cancel()
			}
		}()
	}
	for {
		c.stateMu.Lock()
		deadline, changed := c.writeDeadline, c.writeChanged
		c.stateMu.Unlock()
		if c.ctx.Err() != nil {
			return net.ErrClosed
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return os.ErrDeadlineExceeded
		}
		if !time.Now().Before(readyAt) {
			return nil
		}
		delay := time.Until(readyAt)
		if !deadline.IsZero() && time.Until(deadline) < delay {
			delay = time.Until(deadline)
		}
		timer := time.NewTimer(delay)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return net.ErrClosed
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
