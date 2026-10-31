//go:build linux

package network

import (
	"fmt"
	"net"
	"strings"
)

// Symmetric ICMP is useful when a path admits only requests, or admits
// unsolicited replies. The direction/tag/session validation stays identical
// to xDi; only the outer echo type changes. Ordinary xDi remains unchanged.
func newICMPSymmetricConn(token string, server, request, v6 bool) (net.PacketConn, error) {
	pc, err := listenICMP(v6)
	if err != nil {
		return nil, err
	}
	var c *icmpConn
	if server {
		c = newICMPServerConnWith(pc, token).(*icmpConn)
	} else {
		c = newICMPClientConnWith(pc, token).(*icmpConn)
	}
	if v6 {
		c.proto = 58
	}
	c.symmetricEcho = 2
	if request {
		c.symmetricEcho = 1
	}
	id := -1
	if !server {
		id = int(c.id)
	}
	attachICMPFilter(pc, c.echoType(true), id)
	accept, guard := symmetricICMPRules(c.tag, server, request, v6)
	c.accept = installEchoRule(&icmpEchoGuard{rule: accept, tool: echoFirewall(v6)})
	if request {
		c.echoGuard = installEchoRule(&icmpEchoGuard{rule: guard, tool: echoFirewall(v6)})
	}
	return c, nil
}

func symmetricICMPRules(tag [xdiTagLen]byte, server, request, v6 bool) (accept, guard []string) {
	accept = xdiAcceptRule(tag, server, v6)
	typ := "echo-reply"
	if request {
		typ = "echo-request"
	}
	setType := func(rule []string) {
		for i := range rule {
			if rule[i] == "--icmp-type" || rule[i] == "--icmpv6-type" {
				rule[i+1] = typ
			}
		}
	}
	setType(accept)
	for i := range accept {
		if accept[i] == "--comment" {
			accept[i+1] += "-same-" + typ
		}
	}
	if !request {
		return accept, nil
	}
	guard = xdiEchoRule(tag, v6)
	for i := range guard {
		if guard[i] == "--u32" {
			expr := guard[i+1]
			if at := strings.LastIndex(expr, "=0x"); at >= 0 {
				guard[i+1] = expr[:at] + fmt.Sprintf("=0x%02x", inboundDir(server))
			}
		}
		if guard[i] == "--comment" {
			guard[i+1] += "-same-" + map[bool]string{true: "server", false: "client"}[server]
		}
	}
	return accept, guard
}
