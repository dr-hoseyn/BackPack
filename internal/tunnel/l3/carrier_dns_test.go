package l3

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"golang.org/x/time/rate"
)

func TestDNSCarrierRotatesSourcesWhileKeepingThePeerReceivePort(t *testing.T) {
	listen, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := listen.LocalAddr().(*net.UDPAddr).Port
	listen.Close()
	base := Config{Carrier: CarrierDNS, Mode: ModeListen, Addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Token: "private DNS carrier unit-test token"}
	first, _, err := openDNS(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	base.Mode = ModeDial
	base.DNS = config.DNSConfig{DNSBind: "127.0.0.2", DNSRotate: 1}
	second, peer, err := openDNS(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = first.SetDeadline(time.Now().Add(time.Second))
	_ = second.SetDeadline(time.Now().Add(time.Second))
	prior := 0
	for i := 0; i < 4; i++ {
		data := []byte{byte(i), 31, 42}
		if _, err = second.WriteTo(data, peer); err != nil {
			t.Fatal(err)
		}
		c := second.(*dnsCarrier)
		c.stateMu.Lock()
		source := c.sender.LocalAddr().(*net.UDPAddr).Port
		c.stateMu.Unlock()
		if source == prior {
			t.Fatal("source-port rotation reused the open sending port")
		}
		prior = source
		buf := make([]byte, 128)
		n, from, e := first.ReadFrom(buf)
		if e != nil || !bytes.Equal(buf[:n], data) {
			t.Fatalf("DNS payload did not arrive: %v", e)
		}
		if from.(*net.UDPAddr).Port != port {
			t.Fatal("the rotated source port replaced the peer's fixed receiving port")
		}
		if _, e = first.WriteTo(data, from); e != nil {
			t.Fatal(e)
		}
		n, _, e = second.ReadFrom(buf)
		if e != nil || !bytes.Equal(buf[:n], data) {
			t.Fatalf("DNS return path failed after rotation: %v", e)
		}
	}
}

func TestDNSPacingHonorsNewDeadlinesAndCancellation(t *testing.T) {
	for _, closeInstead := range []bool{false, true} {
		c := &dnsCarrier{limiter: rate.NewLimiter(1, 1), writeChanged: make(chan struct{}), packets: make(chan *dnsPacket)}
		c.ctx, c.cancel = context.WithCancel(context.Background())
		if err := c.waitToSend(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- c.waitToSend() }()
		if closeInstead {
			c.Close()
		} else {
			_ = c.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
		}
		select {
		case err := <-done:
			want := os.ErrDeadlineExceeded
			if closeInstead {
				want = net.ErrClosed
			}
			if !errors.Is(err, want) {
				t.Fatalf("paced writer returned %v, want %v", err, want)
			}
		case <-time.After(300 * time.Millisecond):
			t.Fatal("paced write ignored deadline update or shutdown")
		}
		c.Close()
	}
}

func TestDNSReadDeadlineAndCloseUnblockWaitingReaders(t *testing.T) {
	c := &dnsCarrier{packets: make(chan *dnsPacket, 1), deadlineChanged: make(chan struct{})}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := c.ReadFrom(make([]byte, 16)); done <- err }()
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("updated DNS read deadline did not wake the reader")
	}
	_ = c.SetReadDeadline(time.Time{})
	go func() { _, _, err := c.ReadFrom(make([]byte, 16)); done <- err }()
	c.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("DNS close did not wake the reader")
	}
}
