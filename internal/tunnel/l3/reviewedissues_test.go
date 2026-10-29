package l3

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestReviewedHandshakeCacheRequiresTheOriginalMessage(t *testing.T) {
	send, recv := newLossyPair(nil)
	token := "cache-regression-token"
	tunnel := &Tunnel{cfg: Config{Mode: ModeListen, Token: token}, encap: greEncap{}, carrier: send, log: quietLogger()}
	attempt, err := beginHandshakeFresh(token, 0, "gre", 1)
	if err != nil {
		t.Fatal(err)
	}
	h, body, err := parseHeader(attempt.datagram())
	if err != nil {
		t.Fatal(err)
	}
	tunnel.handleInit(h, body, recv.addr)
	initial := drain(t, recv)
	if len(initial) != 1 {
		t.Fatalf("initial replies %d", len(initial))
	}
	for _, bad := range [][]byte{nil, append(append([]byte(nil), body...), 0)} {
		tunnel.handleInit(h, bad, recv.addr)
		if got := drain(t, recv); len(got) != 0 {
			t.Fatal("modified message received cached response")
		}
	}
	changed := h
	changed.counter++
	tunnel.handleInit(changed, body, recv.addr)
	if len(drain(t, recv)) != 0 {
		t.Fatal("modified counter received cached response")
	}
	tunnel.handleInit(h, body, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2})
	retry := drain(t, recv)
	if len(retry) != 1 || !bytes.Equal(initial[0], retry[0]) {
		t.Fatal("valid retransmission changed")
	}
}

func TestReviewedFECMalformedShardCannotConsumeAValidSlot(t *testing.T) {
	a, _ := newLossyPair(nil)
	raw, err := newFECCarrier(a, FECConfig{Data: 2, Parity: 1})
	if err != nil {
		t.Fatal(err)
	}
	c := raw.(*fecCarrier)
	for _, body := range [][]byte{nil, {255, 255}, {0}} {
		if _, ok := c.absorb(fecKindData, 123, 0, body, a.addr); ok {
			t.Fatal("malformed shard delivered")
		}
	}
	if len(c.groups) != 0 {
		t.Fatal("malformed shards created group state")
	}
	good := []byte{0, 4, 'g', 'o', 'o', 'd'}
	if payload, ok := c.absorb(fecKindData, 123, 0, good, a.addr); !ok || string(payload) != "good" {
		t.Fatal("valid slot poisoned")
	}
}

func TestReviewedMultipathReadDeadlineWakesPendingRead(t *testing.T) {
	a, b := localUDP(t), localUDP(t)
	mp := newMultipathCarrier([]DatagramCarrier{reviewedUDPPath(a), reviewedUDPPath(b)}, nil)
	defer mp.Close()
	done := make(chan error, 1)
	go func() { _, _, err := mp.ReadFrom(make([]byte, 64)); done <- err }()
	time.Sleep(20 * time.Millisecond)
	mp.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	select {
	case err := <-done:
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Fatalf("expected timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending read ignored deadline")
	}
}

func TestReviewedMultipathOnlyLearnsAuthenticatedSources(t *testing.T) {
	for _, fec := range []bool{false, true} {
		a, b := localUDP(t), localUDP(t)
		genuine, stray := localUDP(t), localUDP(t)
		mp := newMultipathCarrier([]DatagramCarrier{&pinnedCarrier{DatagramCarrier: reviewedUDPPath(a)}, &pinnedCarrier{DatagramCarrier: reviewedUDPPath(b)}}, nil)
		var carrier DatagramCarrier = mp
		if fec {
			carrier = &fecCarrier{below: mp}
		}
		buf := make([]byte, 64)
		genuine.WriteTo([]byte("valid"), a.LocalAddr())
		mp.SetReadDeadline(time.Now().Add(time.Second))
		_, from, err := mp.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		logical := confirmCarrierPeer(carrier, from)
		owner := &Tunnel{carrier: carrier, peer: from, log: quietLogger()}
		owner.notePeer(from)
		if _, provisional := owner.peerAddr().(*receivedPath); provisional {
			t.Fatal("authenticated peer stayed pinned to one path")
		}
		stray.WriteTo([]byte("junk"), a.LocalAddr())
		_, untrusted, err := mp.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		// Failed authentication never calls confirmCarrierPeer.
		if _, err = mp.WriteTo([]byte("reply"), logical); err != nil {
			t.Fatal(err)
		}
		if !arrives(genuine, "reply") || arrives(stray, "reply") {
			t.Fatal("untrusted packet redirected path")
		}
		// Recovered bytes do not authenticate the FEC group's source address.
		if confirmCarrierPeer(carrier, &fecRecoveredAddr{untrusted}) != nil {
			t.Fatal("reconstructed source was trusted")
		}
		// A genuinely authenticated roaming source may take over that path.
		logical = confirmCarrierPeer(carrier, untrusted)
		mp.(*multipathCarrier).next.Store(0)
		if _, err = mp.WriteTo([]byte("roamed"), logical); err != nil {
			t.Fatal(err)
		}
		if !arrives(stray, "roamed") {
			t.Fatal("authenticated roaming stopped working")
		}
		mp.Close()
	}
}
func TestReviewedMultipathTunnelCarriesTrafficWithStrayPackets(t *testing.T) {
	for _, withFEC := range []bool{false, true} {
		t.Run(fmt.Sprint(withFEC), func(t *testing.T) {
			var address string
			for i := 0; i < 20; i++ {
				a := localUDP(t)
				port := a.LocalAddr().(*net.UDPAddr).Port
				if port == 65535 {
					a.Close()
					continue
				}
				second, err := net.ListenPacket("udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
				if err != nil {
					a.Close()
					continue
				}
				address = a.LocalAddr().String()
				a.Close()
				second.Close()
				break
			}
			if address == "" {
				t.Fatal("no pair of consecutive test ports")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			left, right := newFakeDevice(1400), newFakeDevice(1400)
			fec := FECConfig{}
			if withFEC {
				fec = FECConfig{Data: 2, Parity: 1}
			}
			listener, err := New(Config{Mode: ModeListen, Addr: address, Token: "reviewed-multipath", LocalIP: "10.10.0.2/30", PeerIP: "10.10.0.1", Multipath: MultipathConfig{Paths: 2}, FEC: fec}, quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			listener.openDevice = func(deviceSpec) (packetDevice, error) { return right, nil }
			start(t, ctx, cancel, listener, right)
			awaitBind(t, listener)
			dialer, err := New(Config{Mode: ModeDial, Addr: address, Token: "reviewed-multipath", LocalIP: "10.10.0.1/30", PeerIP: "10.10.0.2", Multipath: MultipathConfig{Paths: 2}, FEC: fec}, quietLogger())
			if err != nil {
				t.Fatal(err)
			}
			dialer.openDevice = func(deviceSpec) (packetDevice, error) { return left, nil }
			start(t, ctx, cancel, dialer, left)
			awaitSession(t, dialer, 5*time.Second)
			stray := localUDP(t)
			dest, _ := net.ResolveUDPAddr("udp", address)
			for i := 0; i < 8; i++ {
				stray.WriteTo([]byte("not authenticated"), dest)
				want := ipv4Packet(byte(i))
				left.inject <- want
				select {
				case got := <-right.emitted:
					if !bytes.Equal(got, want) {
						t.Fatal("upload changed")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("upload stalled")
				}
				right.inject <- want
				select {
				case got := <-left.emitted:
					if !bytes.Equal(got, want) {
						t.Fatal("download changed")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("download stalled")
				}
			}
		})
	}
}

func reviewedUDPPath(conn *net.UDPConn) *udpCarrier {
	c := &udpCarrier{UDPConn: conn}
	c.enableBatch()
	return c
}
func TestReviewedMultipathDeadlineCanBeExtendedAndCleared(t *testing.T) {
	a, b := localUDP(t), localUDP(t)
	mp := newMultipathCarrier([]DatagramCarrier{reviewedUDPPath(a), reviewedUDPPath(b)}, nil)
	defer mp.Close()
	mp.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	done := make(chan error, 1)
	go func() { _, _, err := mp.ReadFrom(make([]byte, 64)); done <- err }()
	time.Sleep(20 * time.Millisecond)
	mp.SetReadDeadline(time.Now().Add(time.Second))
	mp.SetReadDeadline(time.Time{})
	select {
	case err := <-done:
		t.Fatalf("old deadline survived extension/clear: %v", err)
	case <-time.After(120 * time.Millisecond):
	}
	mp.SetReadDeadline(time.Now().Add(-time.Second))
	select {
	case err := <-done:
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Fatalf("expected timeout: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("updated deadline did not wake read")
	}
}
