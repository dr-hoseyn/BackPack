package l3

import (
	"bytes"
	"crypto/hmac"
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
)

func TestAuthenticatedFECReusePreservesTagsAcrossGroupsAndForgedFrames(t *testing.T) {
	send, recv := newLossyPair(nil)
	cfg := FECConfig{Data: 4, Parity: 2, Auth: true}
	keys := deriveFECKeys("reusable HMAC test", true)
	tx, err := newFECCarrier(send, cfg, keys)
	if err != nil {
		t.Fatal(err)
	}
	rx, err := newFECCarrier(recv, cfg, deriveFECKeys("reusable HMAC test", false))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65535)
	for round := range 20 {
		var payloads [][]byte
		for _, size := range []int{0, 64, 1200, 60000} {
			payload := bytes.Repeat([]byte{byte(round + size)}, size)
			payloads = append(payloads, payload)
			if _, err := tx.WriteTo(payload, recv.addr); err != nil {
				t.Fatal(err)
			}
		}
		for _, frame := range recv.queue {
			body := frame[:len(frame)-fecTagLen]
			want := fecMAC(&keys.send, body) // independent, freshly keyed HMAC
			if !hmac.Equal(want[:fecTagLen], frame[len(body):]) {
				t.Fatal("reused signer changed the wire tag")
			}
		}
		bad := bytes.Clone(recv.queue[0])
		bad[len(bad)-1] ^= 1
		recv.queue = append([][]byte{bad}, recv.queue...)
		for _, want := range payloads {
			n, _, err := rx.ReadFrom(buf)
			if err != nil || !bytes.Equal(buf[:n], want) {
				t.Fatalf("round %d: authentic packet lost after forged MAC: %v", round, err)
			}
		}
		// Consume the remaining parity without delivering data twice.
		if _, _, err := rx.ReadFrom(buf); err == nil {
			t.Fatal("authenticated parity redelivered complete data")
		}
	}
}

func TestAuthenticatedFECSeparateSendAndReceiveState(t *testing.T) {
	a, b := newLossyPair(nil)
	cfg := FECConfig{Data: 4, Parity: 2, Auth: true}
	left, err := newFECCarrier(a, cfg, deriveFECKeys("duplex secret", true))
	if err != nil {
		t.Fatal(err)
	}
	right, err := newFECCarrier(b, cfg, deriveFECKeys("duplex secret", false))
	if err != nil {
		t.Fatal(err)
	}
	// Preload enough complete traffic that readers never depend on scheduling
	// of the concurrent writers. Each carrier then signs and verifies at once.
	for range 100 {
		for _, c := range []DatagramCarrier{left, right} {
			if _, err := c.WriteTo([]byte("preload"), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	var workers sync.WaitGroup
	for _, c := range []DatagramCarrier{left, right} {
		workers.Go(func() {
			for range 100 {
				if _, err := c.WriteTo([]byte("concurrent"), nil); err != nil {
					t.Error(err)
					return
				}
			}
		})
		workers.Go(func() {
			buf := make([]byte, 64)
			for range 100 {
				n, _, err := c.ReadFrom(buf)
				if err != nil || string(buf[:n]) != "preload" {
					t.Errorf("duplex authentication failed: payload=%q err=%v", buf[:n], err)
					return
				}
			}
		})
	}
	workers.Wait()
}

// No-allocation carrier sink and a prerecorded authenticated receive ring.
// The receive ring is wider than fecWindow so every wrapped group is expired.
type fecBenchmarkCarrier struct {
	DatagramCarrier
	frames [][]byte
	next   int
}

func (c *fecBenchmarkCarrier) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (c *fecBenchmarkCarrier) ReadFrom(p []byte) (int, net.Addr, error) {
	frame := c.frames[c.next]
	c.next = (c.next + 1) % len(c.frames)
	return copy(p, frame), nil, nil
}

// This file also compiles against the PR base: record authenticated FEC send
// and receive CPU cost without timing sockets or signing receive fixtures.
func BenchmarkAuthenticatedFEC(b *testing.B) {
	for _, size := range []int{64, 1200} {
		for _, direction := range []string{"send", "receive"} {
			b.Run(fmt.Sprintf("%s/bytes-%d", direction, size), func(b *testing.B) {
				cfg := FECConfig{Data: 4, Parity: 2, Auth: true}
				keys := deriveFECKeys("benchmark key", true)
				below := &fecBenchmarkCarrier{}
				if direction == "receive" {
					for group := range 4 * fecWindow {
						for index := range cfg.Data {
							frame := make([]byte, fecOverhead+size+fecTagLen)
							putFECHeader(frame, fecKindData|fecKindAuth, uint32(group), byte(index))
							binary.BigEndian.PutUint16(frame[fecHeaderLen:], uint16(size))
							tag := fecMAC(&keys.recv, frame[:len(frame)-fecTagLen])
							copy(frame[len(frame)-fecTagLen:], tag[:fecTagLen])
							below.frames = append(below.frames, frame)
						}
					}
				}
				carrier, err := newFECCarrier(below, cfg, keys)
				if err != nil {
					b.Fatal(err)
				}
				payload := make([]byte, size)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					var n int
					var err error
					if direction == "send" {
						n, err = carrier.WriteTo(payload, nil)
					} else {
						n, _, err = carrier.ReadFrom(payload)
					}
					if err != nil || n != size {
						b.Fatalf("invalid authenticated transfer: n=%d err=%v", n, err)
					}
				}
			})
		}
	}
}
