package l3

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestConnectIPAuthenticatesAssignsOnlyPeerAndTransfersRealIP(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6"} {
		t.Run(family, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cfg := Config{Mode: ModeListen, Addr: "127.0.0.1:0", Token: "connect-ip-independent-authentication", Carrier: CarrierConnectIP, Masque: masqueTestConfig(t), LocalIP: "10.205.0.2", PeerIP: "10.205.0.1"}
			if family == "IPv6" {
				cfg.LocalIP, cfg.PeerIP = "fd51::2", "fd51::1"
			}
			ready := make(chan net.Addr, 1)
			accepted := make(chan *connectIPSession, 1)
			failed := make(chan error, 1)
			go func() {
				s, err := acceptConnectIP(ctx, cfg, func(a net.Addr) { ready <- a })
				if err != nil {
					failed <- err
					return
				}
				accepted <- s
			}()
			var address net.Addr
			select {
			case address = <-ready:
			case err := <-failed:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			clientCfg := cfg
			clientCfg.Mode, clientCfg.Addr, clientCfg.LocalIP, clientCfg.PeerIP = ModeDial, address.String(), cfg.PeerIP, cfg.LocalIP
			bad := clientCfg
			bad.Token = "different-connect-ip-authorization"
			if s, err := dialConnectIP(ctx, bad); err == nil {
				s.close()
				t.Fatal("unauthorized CONNECT-IP accepted")
			}
			bad = clientCfg
			bad.Masque.ServerName = "wrong.example"
			if s, err := dialConnectIP(ctx, bad); err == nil {
				s.close()
				t.Fatal("unverified TLS name accepted")
			}
			client, err := dialConnectIP(ctx, clientCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.close()
			var server *connectIPSession
			select {
			case server = <-accepted:
			case err := <-failed:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			defer server.close()
			local, peer := ipEnds(clientCfg)
			transfer := func(sender, receiver *connectIPSession, src, dst netip.Addr) {
				got := make(chan []byte, 1)
				go func() {
					b := make([]byte, 2048)
					n, err := receiver.conn.ReadPacket(b)
					if err != nil {
						got <- nil
						return
					}
					got <- b[:n]
				}()
				if icmp, err := sender.conn.WritePacket(connectIPPing(src, dst)); err != nil || icmp != nil {
					t.Fatalf("write: %v", err)
				}
				select {
				case packet := <-got:
					if !connectIPPacket(packet, src, dst) {
						t.Fatal("not a complete peer IP packet")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			transfer(client, server, local, peer)
			transfer(server, client, peer, local)
		})
	}
}

func TestConnectIPPacketRejectsSpoofedAddressesAndMalformedLengths(t *testing.T) {
	local, peer := netip.MustParseAddr("10.205.0.1"), netip.MustParseAddr("10.205.0.2")
	p := connectIPPing(local, peer)
	if !connectIPPacket(p, local, peer) || connectIPPacket(p, peer, local) || connectIPPacket(p[:len(p)-1], local, peer) {
		t.Fatal("peer packet validation")
	}
	p[0] = 0x4f
	if connectIPPacket(p, local, peer) {
		t.Fatal("accepted truncated IP options")
	}
}

func TestConnectIPRejectsNoiseSpecificTuning(t *testing.T) {
	c := Config{Mode: ModeDial, Addr: "127.0.0.1:443", Token: "long-enough-connect-ip-token", Carrier: CarrierConnectIP, LocalIP: "10.205.0.1/30", PeerIP: "10.205.0.2", MTU: 1280}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.AutoMTU = true
	if c.Validate() == nil {
		t.Fatal("accepted Noise MTU probes for native IP")
	}
}
