package l3

import (
	"bytes"
	"net"
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
	mp := newMultipathCarrier([]DatagramCarrier{&udpCarrier{UDPConn: a}, &udpCarrier{UDPConn: b}}, nil)
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
		mp := newMultipathCarrier([]DatagramCarrier{&pinnedCarrier{DatagramCarrier: &udpCarrier{UDPConn: a}}, &pinnedCarrier{DatagramCarrier: &udpCarrier{UDPConn: b}}}, nil)
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
