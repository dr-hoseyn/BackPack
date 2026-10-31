package kernel

import (
	"fmt"
	"net/netip"
	"strconv"

	"github.com/backpack/backpack/config"
)

// A step has an undo only when it creates a resource. Failed creation never
// schedules deletion: another tunnel's resource must survive an ID collision.
type Step struct{ Args, Undo []string }
type Plan struct {
	Config        config.KernelConfig
	Steps         []Step
	Interfaces    []string
	Local, Remote string
}

func number(n int) string                { return strconv.Itoa(n) }
func id(n uint32) string                 { return strconv.FormatUint(uint64(n), 10) }
func (p *Plan) step(args, undo []string) { p.Steps = append(p.Steps, Step{args, undo}) }
func (p *Plan) link(name string, args ...string) {
	p.Interfaces = append(p.Interfaces, name)
	p.step(append([]string{"link", "add", name}, args...), []string{"link", "del", name})
	p.step([]string{"link", "set", "dev", name, "alias", owner(p.Config)}, nil)
}
func (p *Plan) address(name, addr string, mtu int) {
	p.step([]string{"address", "add", addr, "dev", name}, nil)
	p.step([]string{"link", "set", "dev", name, "mtu", number(mtu), "up"}, nil)
}
func owner(c config.KernelConfig) string { return "backpack-kernel:" + c.Interface }

func carrierAddresses(c config.KernelConfig) (string, string) {
	a, b := 1, 2
	if c.Role == "kharej" {
		a, b = b, a
	}
	if m, _ := Lookup(c.Method); m.IPv6 {
		return fmt.Sprintf("fdba:%x:%x::%d/64", c.ID>>16, c.ID&65535, a), fmt.Sprintf("fdba:%x:%x::%d", c.ID>>16, c.ID&65535, b)
	}
	// /30 within RFC 1918; the shared ID determines a separate carrier subnet.
	n := uint32(0xac100000) + (c.ID&0x3ffff)*4
	ip := func(offset int) string {
		return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n + uint32(offset))}).String()
	}
	return ip(a) + "/30", ip(b)
}

func Build(input config.KernelConfig) (Plan, error) {
	c, err := Resolve(input)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Config: c, Local: c.LocalAddr, Remote: c.RemoteAddr}
	dev := c.Interface
	if c.Reverse && c.Method != "wireguard" {
		local, remote := carrierAddresses(c)
		prefix, _ := netip.ParsePrefix(local)
		p.Local, p.Remote = prefix.Addr().String(), remote
		name := dev + "r"
		p.Interfaces = append(p.Interfaces, name)
		p.step([]string{"tuntap", "add", "dev", name, "mode", "tun"}, []string{"link", "del", name})
		p.step([]string{"link", "set", "dev", name, "alias", owner(c)}, nil)
		p.address(name, local, 9000)
	}
	base := []string{"local", p.Local, "remote", p.Remote}
	kind := c.Method
	switch kind {
	case "wireguard":
		p.link(dev, "type", "wireguard")
	case "eoip", "etherip":
		p.Interfaces = append(p.Interfaces, dev)
		p.step([]string{"tuntap", "add", "dev", dev, "mode", "tap"}, []string{"link", "del", dev})
		p.step([]string{"link", "set", "dev", dev, "alias", owner(c)}, nil)
	case "6to4", "6to4-gre":
		p.link(dev+"s", append([]string{"type", "sit"}, base...)...)
		a, b := 1, 2
		if c.Role == "kharej" {
			a, b = b, a
		}
		local := fmt.Sprintf("fdc6:%x:%x::%d", c.ID>>16, c.ID&65535, a)
		remote := fmt.Sprintf("fdc6:%x:%x::%d", c.ID>>16, c.ID&65535, b)
		p.address(dev+"s", local+"/64", c.MTU+40)
		inner := "ip6tnl"
		extra := []string{"mode", "ipip6"}
		if kind == "6to4-gre" {
			inner = "ip6gre"
			extra = []string{"key", id(c.ID)}
		}
		args := append([]string{"type", inner, "local", local, "remote", remote}, extra...)
		p.link(dev, args...)
	case "gre", "fou-gre", "ecmp-lanes", "ip6gre", "xfrm-ipsec", "gretap", "ip6gretap":
		if kind == "fou-gre" || kind == "ecmp-lanes" || kind == "xfrm-ipsec" {
			kind = "gre"
		}
		if c.Method == "fou-gre" && !c.Reverse {
			p.step([]string{"fou", "add", "port", number(c.Port), "ipproto", "47"}, []string{"fou", "del", "port", number(c.Port)})
		}
		for lane := 0; lane < c.Lanes; lane++ {
			name := dev
			if lane > 0 {
				name += number(lane)
			}
			args := append([]string{"type", kind}, base...)
			args = append(args, "key", id(c.ID+uint32(lane)))
			if c.Method == "fou-gre" {
				port := c.Port
				if c.Reverse {
					port = 5555
					p.step([]string{"fou", "add", "port", number(port), "ipproto", "47", "local", p.Local, "peer", p.Remote}, []string{"fou", "del", "port", number(port), "local", p.Local, "peer", p.Remote})
				}
				args = append(args, "encap", "fou", "encap-sport", "auto", "encap-dport", number(port))
			}
			p.link(name, args...)
			if c.Lanes > 1 {
				prefix, _ := netip.ParsePrefix(c.LocalIP)
				bits := 32
				if prefix.Addr().Is6() {
					bits = 128
				}
				p.address(name, netip.PrefixFrom(prefix.Addr(), bits).String(), c.MTU)
			}
		}
	case "ipip", "sit", "gue-ipip":
		if kind == "gue-ipip" {
			kind = "ipip"
		}
		args := append([]string{"type", kind}, base...)
		if c.Method == "gue-ipip" {
			port := c.Port
			if c.Reverse {
				port = 5556
			}
			p.step([]string{"fou", "add", "port", number(port), "gue", "local", p.Local, "peer", p.Remote}, []string{"fou", "del", "port", number(port), "local", p.Local, "peer", p.Remote})
			args = append(args, "encap", "gue", "encap-sport", "auto", "encap-dport", number(port))
		}
		p.link(dev, args...)
	case "ipip6", "ip6ip6":
		p.link(dev, append(append([]string{"type", "ip6tnl", "mode", kind}, base...), "encaplimit", "none")...)
	case "vxlan":
		port := c.Port
		if c.Reverse {
			port = 4789
		}
		p.link(dev, "type", "vxlan", "id", id(c.ID), "local", p.Local, "remote", p.Remote, "dstport", number(port), "nolearning")
	case "geneve":
		port := c.Port
		if c.Reverse {
			port = 6081
		}
		p.link(dev, "type", "geneve", "id", id(c.ID), "remote", p.Remote, "dstport", number(port))
	case "l2tpv3":
		port := c.Port
		if c.Reverse {
			port = 1701
		}
		p.step(append([]string{"l2tp", "add", "tunnel", "tunnel_id", id(c.ID), "peer_tunnel_id", id(c.ID), "encap", "udp", "udp_sport", number(port), "udp_dport", number(port)}, base...), []string{"l2tp", "del", "tunnel", "tunnel_id", id(c.ID)})
		p.Interfaces = append(p.Interfaces, dev)
		p.step([]string{"l2tp", "add", "session", "tunnel_id", id(c.ID), "session_id", id(c.ID), "peer_session_id", id(c.ID), "name", dev}, []string{"l2tp", "del", "session", "tunnel_id", id(c.ID), "session_id", id(c.ID)})
		p.step([]string{"link", "set", "dev", dev, "alias", owner(c)}, nil)
	}
	if c.Lanes <= 1 {
		if c.Method == "xfrm-ipsec" {
			p.step([]string{"address", "add", c.LocalIP, "dev", dev}, nil)
			p.step([]string{"link", "set", "dev", dev, "mtu", number(c.MTU)}, nil)
		} else {
			p.address(dev, c.LocalIP, c.MTU)
		}
		peer, _ := netip.ParseAddr(c.PeerIP)
		bits := 32
		if peer.Is6() {
			bits = 128
		}
		target := netip.PrefixFrom(peer, bits).String()
		if c.Method != "xfrm-ipsec" {
			p.step([]string{"route", "add", target, "dev", dev, "proto", "static"}, []string{"route", "del", target, "dev", dev, "proto", "static"})
		}
	} else {
		peer, _ := netip.ParseAddr(c.PeerIP)
		bits := 32
		if peer.Is6() {
			bits = 128
		}
		args := []string{"route", "add", netip.PrefixFrom(peer, bits).String(), "proto", "static"}
		for lane := 0; lane < c.Lanes; lane++ {
			name := dev
			if lane > 0 {
				name += number(lane)
			}
			args = append(args, "nexthop", "dev", name, "weight", "1")
		}
		p.step(args, []string{"route", "del", netip.PrefixFrom(peer, bits).String(), "proto", "static"})
	}
	return p, nil
}
