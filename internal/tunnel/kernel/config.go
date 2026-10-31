// Package kernel manages standard Linux L3/L2 tunnels and an optional reverse
// carrier. Network state belongs to a single configuration and is unwound on
// failure or shutdown, in reverse creation order.
package kernel

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/tunnel/l3"
)

type Method struct {
	Name, Family              string
	IPv6, IPv6Only, Encrypted bool
}

var methods = []Method{
	{"gre", "L3", false, false, false}, {"ipip", "L3", false, false, false},
	{"6to4", "L3", false, false, false}, {"6to4-gre", "L3", false, false, false},
	{"sit", "L3", false, true, false}, {"ip6gre", "L3", true, false, false},
	{"ipip6", "L3", true, false, false}, {"ip6ip6", "L3", true, true, false},
	{"wireguard", "L3", false, false, true}, {"xfrm-ipsec", "L3", false, false, true},
	{"fou-gre", "L3", false, false, false}, {"gue-ipip", "L3", false, false, false},
	{"ecmp-lanes", "L3", false, false, false},
	{"eoip", "L2", false, false, false}, {"gretap", "L2", false, false, false},
	{"ip6gretap", "L2", true, false, false}, {"vxlan", "L2", false, false, false},
	{"geneve", "L2", false, false, false}, {"l2tpv3", "L2", false, false, false},
	{"etherip", "L2", false, false, false},
}

func Methods() []Method { return append([]Method(nil), methods...) }
func Lookup(name string) (Method, bool) {
	for _, m := range methods {
		if name == m.Name {
			return m, true
		}
	}
	return Method{}, false
}

var interfaceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,10}$`)

func Resolve(c config.KernelConfig) (config.KernelConfig, error) {
	c.Method, c.Role = strings.ToLower(strings.TrimSpace(c.Method)), strings.ToLower(strings.TrimSpace(c.Role))
	m, ok := Lookup(c.Method)
	if !ok {
		return c, fmt.Errorf("kernel: unknown method %q", c.Method)
	}
	if c.Role != "iran" && c.Role != "kharej" {
		return c, fmt.Errorf("kernel: role must be iran or kharej")
	}
	local, le := netip.ParseAddr(c.LocalAddr)
	remote, re := netip.ParseAddr(c.RemoteAddr)
	if le != nil || re != nil || local.IsUnspecified() || remote.IsUnspecified() || local.IsMulticast() || remote.IsMulticast() || local == remote || local.Zone() != "" || remote.Zone() != "" {
		return c, fmt.Errorf("kernel: distinct literal unicast local_addr and remote_addr are required")
	}
	if c.Method != "wireguard" && (local.Is6() != m.IPv6 || remote.Is6() != m.IPv6) || local.Is6() != remote.Is6() {
		return c, fmt.Errorf("kernel: %s has an incompatible underlay address family", c.Method)
	}
	addr, ae := netip.ParsePrefix(c.LocalIP)
	peer, pe := netip.ParseAddr(c.PeerIP)
	if ae != nil || pe != nil || addr.Addr().IsUnspecified() || peer.IsUnspecified() || addr.Addr().IsMulticast() || peer.IsMulticast() || addr.Addr() == peer || addr.Addr().Is6() != peer.Is6() {
		return c, fmt.Errorf("kernel: local_ip needs a prefix and peer_ip must be a distinct address of the same family")
	}
	if m.IPv6Only && !peer.Is6() || c.Method == "ipip6" && !peer.Is4() {
		return c, fmt.Errorf("kernel: %s cannot carry this inner IP family", c.Method)
	}
	if len(c.Ports) > 0 && (c.Role != "iran" || m.IPv6Only) {
		return c, fmt.Errorf("kernel: forwarded ports require an Iran endpoint that carries IPv4")
	}
	if len(c.Token) < 24 {
		return c, fmt.Errorf("kernel: token must contain at least 24 characters (use a random secret)")
	}
	seed := sha256.Sum256([]byte("backpack-kernel-v1|" + c.Token + "|" + c.Method))
	automaticID := c.ID == 0
	if c.ID == 0 {
		c.ID = binary.BigEndian.Uint32(seed[:4]) & 0xffffff
		automaticID := c.ID == 0
		if c.ID == 0 {
			c.ID = 1
		}
	}
	if c.ID > 0xffffff || (c.Method == "eoip" && c.ID > 65535) {
		if automaticID && c.Method == "eoip" && c.ID <= 0xffffff {
			c.ID = c.ID%65535 + 1
		} else {
			return c, fmt.Errorf("kernel: id is outside the method's range")
		}
	}
	if c.Interface == "" {
		c.Interface = fmt.Sprintf("bpk%x", seed[:4])
	}
	if !interfaceName.MatchString(c.Interface) {
		return c, fmt.Errorf("kernel: interface must be 1..11 letters, digits, underscores or hyphens")
	}
	if c.MTU == 0 {
		c.MTU = 1380
	}
	if c.MTU < 576 || c.MTU > 9000 || peer.Is6() && c.MTU < 1280 {
		return c, fmt.Errorf("kernel: invalid mtu")
	}
	if c.Lanes == 0 {
		c.Lanes = 1
		if c.Method == "ecmp-lanes" {
			c.Lanes = 4
		}
	}
	if c.Lanes < 1 || c.Lanes > 8 || c.Lanes > 1 && c.Method != "gre" && c.Method != "ip6gre" && c.Method != "ecmp-lanes" {
		return c, fmt.Errorf("kernel: lanes must be 1..8 and require GRE or ECMP")
	}
	if c.Port == 0 {
		switch c.Method {
		case "wireguard":
			c.Port = 51820
		case "vxlan":
			c.Port = 4789
		case "geneve":
			c.Port = 6081
		case "fou-gre", "gue-ipip":
			c.Port = 5555
		case "l2tpv3":
			c.Port = 1701
		}
		if c.Reverse && c.Method != "wireguard" {
			c.Port = 24443
		}
	}
	if c.Port != 0 && !c.Reverse && c.Method != "wireguard" && c.Method != "vxlan" && c.Method != "geneve" && c.Method != "fou-gre" && c.Method != "gue-ipip" && c.Method != "l2tpv3" {
		return c, fmt.Errorf("kernel: this native method has no port; use reverse for a TCP carrier")
	}
	if c.Port < 0 || c.Port > 65535 {
		return c, fmt.Errorf("kernel: invalid port")
	}
	if _, err := Forwarder(c); err != nil {
		return c, err
	}
	return c, nil
}

func Forwarder(c config.KernelConfig) (*l3.Forwarder, error) {
	return l3.NewForwarder(l3.Config{PeerIP: c.PeerIP, Ports: c.Ports, AcceptUDP: c.AcceptUDP, MaxConnections: c.MaxConnections, BandwidthMbps: c.BandwidthMbps}, nil)
}

func Peer(c config.KernelConfig) config.KernelConfig {
	if c.Role == "iran" {
		c.Role = "kharej"
	} else {
		c.Role = "iran"
	}
	c.LocalAddr, c.RemoteAddr = c.RemoteAddr, c.LocalAddr
	p, _ := netip.ParsePrefix(c.LocalIP)
	other, _ := netip.ParseAddr(c.PeerIP)
	c.LocalIP, c.PeerIP = netip.PrefixFrom(other, p.Bits()).String(), p.Addr().String()
	c.Ports = nil
	c.AcceptUDP = false
	return c
}
