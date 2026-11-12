//go:build linux

package l3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/tunnel/limits"
)

type udpReplyCountingReader struct {
	net.Conn
	reads int
}

func (r *udpReplyCountingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.Conn.Read(p)
}

func udpReplyPair(t *testing.T, network, host string) (*net.UDPConn, *net.UDPConn) {
	t.Helper()
	addr, err := net.ResolveUDPAddr(network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := net.ListenUDP(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backend.Close() })
	socket, err := net.DialUDP(network, nil, backend.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socket.Close() })
	return socket, backend
}

func TestUDPReplyReadPreservesWholeDatagramsAndPacing(t *testing.T) {
	for _, tc := range []struct{ network, host string }{{"udp4", "127.0.0.1"}, {"udp6", "::1"}} {
		t.Run(tc.network, func(t *testing.T) {
			socket, backend := udpReplyPair(t, tc.network, tc.host)
			waiter, err := newUDPReplyWaiter(socket)
			if err != nil {
				t.Fatal(err)
			}
			pool := udpReplyBufferPool{free: make(chan *[udpBufferSize]byte, 1)}
			limiter := limits.New(limits.Config{BandwidthMbps: 1})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wrapped := &udpReplyCountingReader{Conn: limiter.Wrap(ctx, socket)}
			for _, size := range []int{0, 1, 1200, 60000, 7} {
				want := bytes.Repeat([]byte{byte(size)}, size)
				if _, err := backend.WriteToUDP(want, socket.LocalAddr().(*net.UDPAddr)); err != nil {
					t.Fatal(err)
				}
				if err := wrapped.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
					t.Fatal(err)
				}
				buf, n, err := readUDPReply(waiter, wrapped, &pool)
				if err != nil {
					t.Fatal(err)
				}
				if n != len(want) || !bytes.Equal(buf[:n], want) {
					t.Fatalf("payload changed: received %d, want %d", n, size)
				}
				pool.put(buf)
			}
			// Readiness checks must not bypass the bandwidth wrapper or
			// consume the actual packet. Its budget is shared with other flows.
			if wrapped.reads != 5 {
				t.Fatal("reply reads bypassed the connection wrapper")
			}
			cancel()
			if _, err := backend.WriteToUDP([]byte("cancelled"), socket.LocalAddr().(*net.UDPAddr)); err != nil {
				t.Fatal(err)
			}
			if buf, _, err := readUDPReply(waiter, wrapped, &pool); buf != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("reply read bypassed pacing cancellation: %v", err)
			}
		})
	}
}

func TestUDPReplyConcurrentFlowsKeepPayloadsIsolated(t *testing.T) {
	backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		buf := make([]byte, udpBufferSize)
		for {
			n, addr, err := backend.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if _, err := backend.WriteToUDPAddrPort(buf[:n], addr); err != nil {
				return
			}
		}
	}()
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	startForwarder(t, []string{addr + "=" + backend.LocalAddr().String()}, true)
	var workers sync.WaitGroup
	for i := range 16 {
		workers.Go(func() {
			client, err := net.Dial("udp4", addr)
			if err != nil {
				t.Error(err)
				return
			}
			defer client.Close()
			buf := make([]byte, 1200)
			for seq := range 20 {
				want := bytes.Repeat([]byte{byte(i), byte(seq)}, 600)
				_ = client.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := client.Write(want); err != nil {
					t.Error(err)
					return
				}
				n, err := client.Read(buf)
				if err != nil || !bytes.Equal(buf[:n], want) {
					t.Errorf("flow %d reply %d was lost or crossed: bytes=%d err=%v", i, seq, n, err)
					return
				}
			}
		})
	}
	workers.Wait()
	backend.Close()
	<-echoDone
}

func TestUDPReplyIdleReadersDoNotHoldPayloadBuffers(t *testing.T) {
	socket, _ := udpReplyPair(t, "udp4", "127.0.0.1")
	waiter, err := newUDPReplyWaiter(socket)
	if err != nil {
		t.Fatal(err)
	}
	pool := udpReplyBufferPool{free: make(chan *[udpBufferSize]byte, 1)}
	pool.put(new([udpBufferSize]byte))
	_ = socket.SetReadDeadline(time.Now().Add(2 * time.Second))
	done := make(chan error, 1)
	go func() {
		buf, _, err := readUDPReply(waiter, socket, &pool)
		if buf != nil {
			pool.put(buf)
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("idle read ended early: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if len(pool.free) != 1 {
		t.Error("an idle socket borrowed a full datagram buffer")
	}
	socket.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("close did not interrupt readiness: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed socket retained its reply worker")
	}
}

func TestUDPReplyReadDeadlineCanBeRenewed(t *testing.T) {
	socket, backend := udpReplyPair(t, "udp4", "127.0.0.1")
	waiter, err := newUDPReplyWaiter(socket)
	if err != nil {
		t.Fatal(err)
	}
	pool := udpReplyBufferPool{free: make(chan *[udpBufferSize]byte, 1)}
	_ = socket.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if buf, _, err := readUDPReply(waiter, socket, &pool); buf != nil || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("idle deadline not respected: %v", err)
	}
	_ = socket.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := backend.WriteToUDP([]byte("renewed"), socket.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	buf, n, err := readUDPReply(waiter, socket, &pool)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.put(buf)
	if string(buf[:n]) != "renewed" {
		t.Fatal("deadline renewal lost the next datagram")
	}
}

func TestUDPReplyBufferCacheRemainsBoundedAfterBurst(t *testing.T) {
	pool := udpReplyBufferPool{free: make(chan *[udpBufferSize]byte, udpReplyBufferCache)}
	borrowed := make([]*[udpBufferSize]byte, 2*udpReplyBufferCache)
	for i := range borrowed {
		borrowed[i] = pool.get()
	}
	for _, buf := range borrowed {
		pool.put(buf)
	}
	if len(pool.free) != udpReplyBufferCache {
		t.Fatal("burst storage exceeded the fixed cache budget")
	}
}
