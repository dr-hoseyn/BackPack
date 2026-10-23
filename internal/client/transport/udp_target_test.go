package transport

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils"
)

func TestRawUDPTargetPreservesLongNamesAndBackendLists(t *testing.T) {
	for _, target := range []string{
		strings.Repeat("long-label.", 12) + "example.com:8443",
		strings.Repeat("127.0.0.1:8443|", 20) + "[::1]:8443",
		"[fe80:1234:5678:9abc:def0:1234:5678:9abc%long-zone-name]:8443",
	} {
		t.Run(target, func(t *testing.T) {
			listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			conn, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.Write([]byte("ready")); err != nil {
				t.Fatal(err)
			}
			var ready [8]byte
			_, peer, err := listener.ReadFromUDP(ready[:])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := listener.WriteToUDP([]byte{utils.SG_Ping}, peer); err != nil {
				t.Fatal(err)
			}
			if _, err := listener.WriteToUDP([]byte(target), peer); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			client := &UdpTransport{}
			port, got, err := client.udpTarget(conn)
			if err != nil || port != 8443 || got != target {
				t.Fatalf("UDP target = %q, port = %d, error = %v; want %q", got, port, err, target)
			}
		})
	}
}
