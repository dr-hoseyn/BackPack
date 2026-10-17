package network

import (
	"fmt"
	"net"

	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// IPv6 raw TCP keeps sequence/options in userspace; the kernel owns IPv6
// routing and neighbour discovery. No connected TCP socket is used.
func discoverEgress6(dst net.IP, override string) (*pckEgress, error) {
	var local net.IP
	conn, err := net.DialUDP("udp6", nil, &net.UDPAddr{IP: dst, Port: 9})
	if err == nil {
		local = conn.LocalAddr().(*net.UDPAddr).IP
		conn.Close()
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || override != "" && iface.Name != override {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip.To4() != nil || ip.To16() == nil {
				continue
			}
			if local != nil && !ip.Equal(local) {
				continue
			}
			if local == nil && (ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLoopback() && !dst.IsLoopback()) {
				continue
			}
			return &pckEgress{Iface: &iface, LocalIP: ip, LinkLen: 0}, nil
		}
	}
	return nil, fmt.Errorf("pck: no usable IPv6 source/interface toward %s (override %q)", dst, override)
}
func (c *pckConn) IPHeaderLen() int {
	if c.egress.LocalIP.To4() == nil {
		return 40
	}
	return 20
}
func (c *pckConn) openTx6() error {
	pc, err := net.ListenPacket("ip6:tcp", c.egress.LocalIP.String())
	if err != nil {
		return err
	}
	if err = bindPacketConnToInterface(pc, c.egress.Iface.Name); err != nil {
		pc.Close()
		return err
	}
	p6 := ipv6.NewPacketConn(pc)
	if err = p6.SetHopLimit(64); err != nil {
		pc.Close()
		return err
	}
	_ = p6.SetTrafficClass(46 << 2)
	if raw, err := pc.(*net.IPConn).SyscallConn(); err == nil {
		_ = raw.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_MTU_DISCOVER, unix.IPV6_PMTUDISC_DO)
		})
	}
	c.txRawPC, c.tx6 = pc, p6
	return nil
}
func (c *pckConn) write6(p []byte, dst *net.UDPAddr, seq, ack, tsecr uint32) (int, error) {
	if dst.IP.To4() != nil || dst.IP.To16() == nil {
		return 0, net.InvalidAddrError("pck: IPv6 destination required")
	}
	f := c.flags[int(c.flagRot.Add(1)-1)%len(c.flags)]
	tcp := buildPckTCP(c.local, uint16(dst.Port), seq, ack, f, c.timestamp(), tsecr, c.egress.LocalIP, dst.IP, p)
	_, err := c.tx6.WriteTo(tcp, nil, &net.IPAddr{IP: dst.IP, Zone: dst.Zone})
	if err != nil {
		return c.writeFailed(len(p), err)
	}
	return len(p), nil
}
