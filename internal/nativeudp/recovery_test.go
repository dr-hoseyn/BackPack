package nativeudp

import (
	"net"
	"testing"
	"time"
)

func TestNativeDatagramRekeysWithoutReplacingItsPacketAssociation(t *testing.T) {
	s, c, _, _ := linkPair(t)
	c.mu.Lock()
	old := c.active.id
	c.active.created = time.Now().Add(-rekeyAfter - time.Second)
	c.mu.Unlock()
	deadline := time.Now().Add(4 * time.Second)
	for {
		c.mu.Lock()
		changed := c.active != nil && c.active.id != old
		c.mu.Unlock()
		s.mu.Lock()
		paired := s.active != nil && s.active.id != old
		s.mu.Unlock()
		if changed && paired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic Noise rekey did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Write([]byte("same association after rekey")); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 100)
		n, err := s.Read(b)
		if err != nil {
			got <- err.Error()
		} else {
			got <- string(b[:n])
		}
	}()
	select {
	case b := <-got:
		if b != "same association after rekey" {
			t.Fatal(b)
		}
	case <-time.After(time.Second):
		t.Fatal("rekey stalled payload delivery")
	}
}

func TestNativeDatagramWrongTokenNeverAuthenticatesOrQueuesApplicationData(t *testing.T) {
	a, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	s, err := NewLink(t.Context(), a, nil, "one-native-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := NewLink(t.Context(), b, a.LocalAddr(), "different-native-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(1100 * time.Millisecond)
	if s.Stats().Connected || c.Stats().Connected {
		t.Fatal("wrong-token association authenticated")
	}
	if err = c.Write([]byte("must be dropped")); err == nil {
		t.Fatal("unauthenticated payload accepted")
	}
	if s.Stats().Received != 0 {
		t.Fatal("unauthenticated application payload delivered")
	}
}
