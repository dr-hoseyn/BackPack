//go:build linux

package network

import (
	"fmt"
	"strings"
	"testing"
)

func TestSymmetricICMPTypesPreserveDirectionAndScopedFirewallRules(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		for _, server := range []bool{false, true} {
			for _, request := range []bool{false, true} {
				c := &icmpConn{server: server, proto: 1, symmetricEcho: 2}
				if request {
					c.symmetricEcho = 1
				}
				if v6 {
					c.proto = 58
				}
				want := uint8(0)
				if request {
					want = 8
				}
				if v6 {
					want += 129
					if request {
						want = 128
					}
				}
				if c.echoType(true) != want || c.echoType(false) != want {
					t.Fatal("a symmetric carrier still used xDi's split echo types")
				}
				accept, guard := symmetricICMPRules(xdiTag("symmetric-test"), server, request, v6)
				for _, arg := range accept {
					if strings.Contains(arg, "&&") && !strings.HasSuffix(arg, fmt.Sprintf("=0x%02x", inboundDir(server))) {
						t.Fatalf("accept rule lost its direction: %v", accept)
					}
				}
				if request {
					joined := strings.Join(guard, " ")
					if !strings.Contains(joined, "echo-reply") || !strings.Contains(joined, fmt.Sprintf("=0x%02x", inboundDir(server))) {
						t.Fatalf("kernel auto-reply guard matches the wrong direction: %v", guard)
					}
				} else if guard != nil {
					t.Fatal("reply-only mode installed an unnecessary ping suppression rule")
				}
			}
		}
	}
}
