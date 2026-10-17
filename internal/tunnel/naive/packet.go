package naive

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// SOCKSPacketConn uses SOCKS5 UDP ASSOCIATE only on literal loopback. The
// helper carries native Hysteria2/TUIC datagrams; there is no UDP-over-stream
// fallback. A helper restart loses datagrams while its association reconnects.
type SOCKSPacketConn struct {
	*net.UDPConn
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	mu       sync.Mutex
	control  net.Conn
	relay    *net.UDPAddr
	proxy    string
	target   *net.UDPAddr
	header   []byte
	rmu, wmu sync.Mutex
	rx, tx   [65535]byte
}

func NewSOCKSPacketConn(ctx context.Context, proxy, target string) (*SOCKSPacketConn, error) {
	if _, _, err := hostPort(proxy, true); err != nil {
		return nil, err
	}
	if _, _, err := hostPort(target, true); err != nil {
		return nil, err
	}
	dest, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &SOCKSPacketConn{UDPConn: udp, ctx: ctx, cancel: cancel, done: make(chan struct{}), proxy: proxy, target: dest}
	c.header = []byte{0, 0, 0}
	if ip := dest.IP.To4(); ip != nil {
		c.header = append(c.header, 1)
		c.header = append(c.header, ip...)
	} else {
		c.header = append(c.header, 4)
		c.header = append(c.header, dest.IP.To16()...)
	}
	c.header = binary.BigEndian.AppendUint16(c.header, uint16(dest.Port))
	if err := c.associate(); err != nil {
		cancel()
		udp.Close()
		return nil, err
	}
	go c.maintain()
	return c, nil
}

func (c *SOCKSPacketConn) associate() error {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	control, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.proxy)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { control.Close() })
	defer stop()
	control.SetDeadline(time.Now().Add(5 * time.Second))
	fail := func(err error) error { control.Close(); return err }
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	var auth [2]byte
	if _, err := io.ReadFull(control, auth[:]); err != nil {
		return fail(err)
	}
	if auth != [2]byte{5, 0} {
		return fail(errors.New("SOCKS UDP authentication refused"))
	}
	// Zero address lets the helper learn the actual local UDP sender. The TCP
	// association stays open for precisely the lifetime of that helper process.
	if _, err := control.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return fail(err)
	}
	var h [4]byte
	if _, err := io.ReadFull(control, h[:]); err != nil {
		return fail(err)
	}
	if h[0] != 5 || h[1] != 0 || h[2] != 0 {
		return fail(errors.New("SOCKS UDP association refused"))
	}
	var ip net.IP
	switch h[3] {
	case 1:
		ip = make(net.IP, 4)
	case 4:
		ip = make(net.IP, 16)
	default:
		return fail(errors.New("SOCKS UDP relay must return a literal loopback IP"))
	}
	if _, err := io.ReadFull(control, ip); err != nil {
		return fail(err)
	}
	var p [2]byte
	if _, err := io.ReadFull(control, p[:]); err != nil {
		return fail(err)
	}
	if ip.IsUnspecified() {
		ip = net.IPv4(127, 0, 0, 1)
	}
	if !ip.IsLoopback() || binary.BigEndian.Uint16(p[:]) == 0 {
		return fail(errors.New("SOCKS UDP relay is not a loopback endpoint"))
	}
	if err := c.ctx.Err(); err != nil {
		return fail(err)
	}
	control.SetDeadline(time.Time{})
	c.mu.Lock()
	c.control, c.relay = control, &net.UDPAddr{IP: ip, Port: int(binary.BigEndian.Uint16(p[:]))}
	c.mu.Unlock()
	return nil
}

func (c *SOCKSPacketConn) maintain() {
	defer close(c.done)
	stop := context.AfterFunc(c.ctx, func() {
		c.mu.Lock()
		if c.control != nil {
			c.control.Close()
		}
		c.mu.Unlock()
		c.UDPConn.Close()
	})
	defer stop()
	for c.ctx.Err() == nil {
		c.mu.Lock()
		control := c.control
		c.mu.Unlock()
		var b [1]byte
		_, _ = control.Read(b[:])
		control.Close()
		c.mu.Lock()
		c.relay = nil
		c.mu.Unlock()
		for c.ctx.Err() == nil {
			if !sleep(c.ctx, time.Second) {
				return
			}
			if c.associate() == nil {
				break
			}
		}
	}
}

func (c *SOCKSPacketConn) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.mu.Lock()
		if c.control != nil {
			c.control.Close()
		}
		c.mu.Unlock()
		c.UDPConn.Close()
		<-c.done
	})
	return nil
}

func (c *SOCKSPacketConn) WriteTo(p []byte, dest net.Addr) (int, error) {
	a, ok := dest.(*net.UDPAddr)
	if !ok || a.Port != c.target.Port || a.Zone != c.target.Zone || !a.IP.Equal(c.target.IP) {
		return 0, errors.New("SOCKS UDP target is fixed")
	}
	if len(p)+len(c.header) > len(c.tx) {
		return 0, errors.New("SOCKS UDP datagram too large")
	}
	if c.ctx.Err() != nil {
		return 0, net.ErrClosed
	}
	c.mu.Lock()
	relay := c.relay
	c.mu.Unlock()
	if relay == nil {
		return len(p), nil
	} // An outage is loss, never reliable buffering.
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n := copy(c.tx[:], c.header)
	n += copy(c.tx[n:], p)
	_, err := c.UDPConn.WriteToUDP(c.tx[:n], relay)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *SOCKSPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		n, from, err := c.UDPConn.ReadFromUDP(c.rx[:])
		if err != nil {
			return 0, nil, err
		}
		c.mu.Lock()
		relay := c.relay
		c.mu.Unlock()
		if relay == nil || !from.IP.Equal(relay.IP) || from.Port != relay.Port || n < len(c.header) {
			continue
		}
		// No fragments, domains, changed destinations or malformed reserved bytes.
		ok := true
		for i, b := range c.header {
			if c.rx[i] != b {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		return copy(p, c.rx[len(c.header):n]), c.target, nil
	}
}
