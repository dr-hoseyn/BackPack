package network

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"golang.org/x/net/dns/dnsmessage"
)

const dnsCarrierMTU = 1100

// dnsPacketConn is a direct UDP carrier with DNS TXT framing. It is not a
// recursive-resolver tunnel: the peer's UDP port must be reachable directly.
type dnsPacketConn struct {
	net.PacketConn
	name     dnsmessage.Name
	response bool
	sequence atomic.Uint32
	readMu   sync.Mutex
	scratch  [4096]byte
}

func newDNSPacketConn(addr, token string, response bool) (*dnsPacketConn, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	h := hmac.New(sha256.New, []byte(token))
	h.Write([]byte("backpack-dns-v1"))
	name, err := dnsmessage.NewName(hex.EncodeToString(h.Sum(nil)[:8]) + ".dns.")
	if err != nil {
		pc.Close()
		return nil, err
	}
	return &dnsPacketConn{PacketConn: pc, name: name, response: response}, nil
}
func (c *dnsPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if len(p) > dnsCarrierMTU {
		return 0, fmt.Errorf("DNS carrier payload exceeds %d bytes", dnsCarrierMTU)
	}
	txt := []string{}
	for b := p; len(b) > 0; {
		n := min(len(b), 255)
		txt = append(txt, string(b[:n]))
		b = b[n:]
	}
	if len(txt) == 0 {
		txt = []string{""}
	}
	m := dnsmessage.Message{Header: dnsmessage.Header{ID: uint16(c.sequence.Add(1)), Response: c.response}, Questions: []dnsmessage.Question{{Name: c.name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}}}
	rr := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: c.name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 0}, Body: &dnsmessage.TXTResource{TXT: txt}}
	if c.response {
		m.Answers = []dnsmessage.Resource{rr}
	} else {
		m.Additionals = []dnsmessage.Resource{rr}
	}
	b, err := m.Pack()
	if err != nil {
		return 0, err
	}
	if _, err = c.PacketConn.WriteTo(b, addr); err != nil {
		return 0, err
	}
	return len(p), nil
}
func (c *dnsPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		n, addr, err := c.PacketConn.ReadFrom(c.scratch[:])
		if err != nil {
			return 0, addr, err
		}
		var m dnsmessage.Message
		if err := m.Unpack(c.scratch[:n]); err != nil {
			continue
		}
		if m.Header.Response == c.response || len(m.Questions) != 1 || m.Questions[0].Name != c.name || m.Questions[0].Type != dnsmessage.TypeTXT {
			continue
		}
		rr := m.Additionals
		if m.Header.Response {
			rr = m.Answers
		}
		if len(rr) != 1 || rr[0].Header.Name != c.name {
			continue
		}
		txt, ok := rr[0].Body.(*dnsmessage.TXTResource)
		if !ok {
			continue
		}
		size := 0
		for _, s := range txt.TXT {
			size += len(s)
		}
		if size > dnsCarrierMTU {
			continue
		}
		if size > len(p) {
			return 0, addr, io.ErrShortBuffer
		}
		at := 0
		for _, s := range txt.TXT {
			at += copy(p[at:], s)
		}
		return at, addr, nil
	}
}
