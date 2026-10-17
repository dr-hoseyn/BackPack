//go:build linux

package amneziawg

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/device"
	"github.com/amnezia-vpn/amneziawg-go/v3/tun/tuntest"
)

func startTestDevice(t *testing.T, c Config) (*device.Device, *tuntest.ChannelTUN, string) {
	t.Helper()
	tun := tuntest.NewChannelTUN()

	// Both test devices use ephemeral loopback UDP ports, never a public listener.
	c.ListenPort = 1
	c.Endpoint = ""
	c.Keepalive = 0
	ipc, err := c.ipcConfig()
	if err != nil {
		t.Fatal(err)
	}
	ipc = strings.Replace(ipc, "listen_port=1\n", "listen_port=0\n", 1)
	d, err := newConfiguredDevice(tun.TUN(), ipc, &device.Logger{Verbosef: func(string, ...any) {}, Errorf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.Up(); err != nil {
		t.Fatal(err)
	}
	status, err := d.IpcGet()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(status, "\n") {
		if port, ok := strings.CutPrefix(line, "listen_port="); ok {
			return d, tun, net.JoinHostPort("127.0.0.1", port)
		}
	}
	t.Fatal("missing listen port")
	return nil, nil, ""
}

func packet(src, dst string) []byte {
	a := netip.MustParsePrefix(src).Addr()
	b := netip.MustParsePrefix(dst).Addr()
	if a.Is6() {
		p := make([]byte, 168)
		p[0] = 0x60
		binary.BigEndian.PutUint16(p[4:6], 128)
		p[6] = 59
		p[7] = 64
		copy(p[8:24], a.AsSlice())
		copy(p[24:40], b.AsSlice())
		return p
	}
	p := make([]byte, 148)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], 148)
	p[8] = 64
	p[9] = 253
	copy(p[12:16], a.AsSlice())
	copy(p[16:20], b.AsSlice())
	return p
}

func TestAmneziaWGEncryptedPacketsAndPeerAuthentication(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6"} {
		t.Run(family, func(t *testing.T) {
			a, b := testPair(t)
			if family == "IPv6" {
				a.Address, a.PeerAddress = "fd00:254:248::1/128", "fd00:254:248::2/128"
				b.Address, b.PeerAddress = a.PeerAddress, a.Address
			}
			da, ta, pa := startTestDevice(t, a)
			db, tb, pb := startTestDevice(t, b)
			for _, item := range []struct {
				d             *device.Device
				key, endpoint string
			}{{da, a.PeerPublicKey, pb}, {db, b.PeerPublicKey, pa}} {
				key, _ := keyBytes(item.key, "public_key")
				if err := item.d.IpcSet(fmt.Sprintf("public_key=%x\nendpoint=%s\n", key, item.endpoint)); err != nil {
					t.Fatal(err)
				}
			}
			for _, direction := range []struct {
				from, to *tuntest.ChannelTUN
				src, dst string
			}{{ta, tb, a.Address, b.Address}, {tb, ta, b.Address, a.Address}} {
				want := packet(direction.src, direction.dst)
				select {
				case direction.from.Outbound <- want:
				case <-time.After(3 * time.Second):
					t.Fatal("send stalled")
				}
				select {
				case got := <-direction.to.Inbound:
					if !bytes.Equal(got, want) {
						t.Fatal("packet corrupted")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("authenticated packet did not arrive")
				}
			}
			// A source outside this peer's AllowedIPs must never reach the TUN.
			source := "10.254.249.1/32"
			if family == "IPv6" {
				source = "fd00:254:249::1/128"
			}
			select {
			case ta.Outbound <- packet(source, b.Address):
			case <-time.After(time.Second):
				t.Fatal("send stalled")
			}
			select {
			case <-tb.Inbound:
				t.Fatal("accepted spoofed source IP")
			case <-time.After(150 * time.Millisecond):
			}
		})
	}
}

func TestAmneziaWGWrongPresharedKeyCannotDeliverPackets(t *testing.T) {
	a, b := testPair(t)
	b.PresharedKey, _ = freshKey()
	da, ta, _ := startTestDevice(t, a)
	_, tb, pb := startTestDevice(t, b)
	key, _ := keyBytes(a.PeerPublicKey, "public_key")
	if err := da.IpcSet(fmt.Sprintf("public_key=%x\nendpoint=%s\n", key, pb)); err != nil {
		t.Fatal(err)
	}
	select {
	case ta.Outbound <- packet(a.Address, b.Address):
	case <-time.After(time.Second):
		t.Fatal("send stalled")
	}
	select {
	case <-tb.Inbound:
		t.Fatal("unauthenticated packet delivered")
	case <-time.After(250 * time.Millisecond):
	}
}
