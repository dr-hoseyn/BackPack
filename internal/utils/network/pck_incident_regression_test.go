//go:build linux

package network

import (
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func incidentNamespace(t *testing.T) {
	t.Helper()
	if os.Getenv("BACKPACK_PCK_NAMESPACE") != "1" || os.Geteuid() != 0 {
		t.Skip("requires the isolated root network namespace")
	}
}

func incidentCarrier(t *testing.T, server bool, token string, port uint16) *pckConn {
	t.Helper()
	incidentNamespace(t)
	pc, err := newPckConn(server, port, PcapCarrier{Port: port, Token: token, Interface: "bp-test", GatewayMAC: "02:00:00:00:00:02", PeerIP: "192.0.2.2"})
	if err != nil {
		t.Fatal(err)
	}
	c := pc.(*pckConn)
	if !c.GuardInstalled() {
		c.Close()
		t.Fatal("carrier has no complete guard")
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func incidentRules(t *testing.T, id string) int {
	t.Helper()
	n := 0
	for _, table := range []string{"filter", "raw"} {
		out, err := exec.Command("iptables", "-t", table, "-S").CombinedOutput()
		if err != nil {
			t.Fatalf("list rules: %v %s", err, out)
		}
		n += len(tunnelRuleDeletions(string(out), pckRulePrefix(id)))
	}
	return n
}

func TestPckIncidentLoopbackResetReachesBackend(t *testing.T) {
	c := incidentCarrier(t, false, "incident-loopback-reset-token", 52777)
	lo := net.IPv4(127, 0, 0, 1).To4()
	rx, err := net.ListenPacket("ip4:tcp", lo.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rx.Close()
	tx, err := net.DialIP("ip4:tcp", &net.IPAddr{IP: lo}, &net.IPAddr{IP: lo})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Close()
	// A stale backend ACK to a closed loopback source port must receive the
	// kernel's reset. This is the reset suppressed in the production incident.
	ack := buildPckTCP(22222, c.local, 41, 73, FlagACK, 111, 0, lo, lo, nil)
	if _, err := tx.Write(ack); err != nil {
		t.Fatal(err)
	}
	rx.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 4096)
	for {
		n, _, err := rx.ReadFrom(buf)
		if err != nil {
			t.Fatalf("backend did not receive the required loopback RST: %v", err)
		}
		if n >= 20 && binary.BigEndian.Uint16(buf[:2]) == c.local && binary.BigEndian.Uint16(buf[2:4]) == 22222 && buf[13]&byte(FlagRST) != 0 {
			return
		}
	}
}

func TestPckIncidentPortCannotBeReusedByKernelTCP(t *testing.T) {
	for _, server := range []bool{false, true} {
		t.Run(strconv.FormatBool(server), func(t *testing.T) {
			c := incidentCarrier(t, server, "incident-port-reservation-token", 52778)
			ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: c.egress.LocalIP, Port: int(c.local)})
			if err == nil {
				ln.Close()
				t.Fatal("ordinary TCP could claim a live PCK source port")
			}
		})
	}
}

func TestPckIncidentQuotedTagCanBeDeleted(t *testing.T) {
	incidentNamespace(t)
	id := "abcde123"
	rule := pckRules(id, 45000, 45127)[0]
	if out, err := exec.Command("iptables", append([]string{"-t", rule[0], "-I"}, rule[1:]...)...).CombinedOutput(); err != nil {
		t.Fatalf("insert fixture: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("iptables", append([]string{"-t", rule[0], "-D"}, rule[1:]...)...).Run() })
	out, err := exec.Command("iptables", "-t", "filter", "-S").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, del := range tunnelRuleDeletions(string(out), pckRulePrefix(id)) {
		for i := range del {
			if del[i] == "--comment" && strings.Contains(del[i+1], `"`) {
				t.Fatal("iptables listing quotes were passed as literal comment characters")
			}
		}
		if out, err := exec.Command("iptables", append([]string{"-t", "filter"}, del...)...).CombinedOutput(); err != nil {
			t.Fatalf("cannot delete quoted tagged rule: %v %s", err, out)
		}
	}
	if incidentRules(t, id) != 0 {
		t.Fatal("tagged rule survived cleanup")
	}
}
