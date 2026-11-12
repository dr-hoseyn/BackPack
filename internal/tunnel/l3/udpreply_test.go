package l3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/tunnel/limits"
)

type udpDeadlineConn struct {
	net.Conn
	read      func([]byte) (int, error)
	deadlines []time.Time
}

func (c *udpDeadlineConn) Read(p []byte) (int, error) { return c.read(p) }
func (c *udpDeadlineConn) SetReadDeadline(at time.Time) error {
	c.deadlines = append(c.deadlines, at)
	return c.Conn.SetReadDeadline(at)
}

func TestUDPReplyTimersDoNotChurnForEveryUncappedPacket(t *testing.T) {
	for _, capped := range []bool{false, true} {
		t.Run(fmt.Sprintf("capped-%t", capped), func(t *testing.T) {
			local, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			a, b := net.Pipe()
			defer b.Close()
			backend := &udpDeadlineConn{Conn: a}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			member := &backendMember{}
			member.active.Store(1)
			flow := &udpFlow{backend: backend, ctx: ctx, cancel: cancel, member: member}
			flow.touch()
			payloads := [][]byte{nil, []byte("x"), bytes.Repeat([]byte("m"), 1200), bytes.Repeat([]byte("l"), 60000)}
			reads := 0
			var renewedFrom int64
			backend.read = func(p []byte) (int, error) {
				reads++
				if reads == 1 {
					// The original timer fires while outbound client traffic
					// still keeps the conversation alive. No payload was read.
					flow.touch()
					renewedFrom = flow.lastSeen.Load()
					return 0, os.ErrDeadlineExceeded
				}
				if reads-2 < len(payloads) {
					return copy(p, payloads[reads-2]), nil
				}
				return 0, io.EOF
			}
			f := &Forwarder{}
			if capped {
				f.limiter = limits.New(limits.Config{BandwidthMbps: 1})
			}
			f.stats.active.Store(1)
			var flows sync.Map
			clientAddr := client.LocalAddr().(*net.UDPAddr).AddrPort()
			flows.Store(clientAddr, flow)
			f.pumpUDPReplies(&flows, flow, local, clientAddr)
			wantTimers := 2 // initial wait and renewal after the injected timeout
			if capped {
				wantTimers = reads // retain the full pacing budget on every read
			}
			if len(backend.deadlines) != wantTimers {
				t.Fatalf("%d deadline resets for %d reads, want %d", len(backend.deadlines), reads, wantTimers)
			}
			if !capped && !backend.deadlines[1].Equal(time.Unix(0, renewedFrom).Add(udpFlowIdle)) {
				t.Fatal("renewal did not preserve the actual idle expiration")
			}
			buf := make([]byte, udpBufferSize)
			_ = client.SetReadDeadline(time.Now().Add(time.Second))
			for _, want := range payloads {
				n, _, err := client.ReadFromUDPAddrPort(buf)
				if err != nil || !bytes.Equal(buf[:n], want) {
					t.Fatalf("reply changed: bytes=%d want=%d err=%v", n, len(want), err)
				}
			}
			if f.Stats().Active != 0 || member.active.Load() != 0 || ctx.Err() == nil {
				t.Fatal("reply worker did not release its flow on exit")
			}
		})
	}
}

func TestUDPReplyIdleTimeoutDoesNotKeepDeadFlowsAlive(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	backend := &udpDeadlineConn{Conn: a, read: func([]byte) (int, error) { return 0, os.ErrDeadlineExceeded }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	member := &backendMember{}
	member.active.Store(1)
	flow := &udpFlow{backend: backend, ctx: ctx, cancel: cancel, member: member}
	flow.lastSeen.Store(time.Now().Add(-2 * udpFlowIdle).UnixNano())
	f := &Forwarder{}
	f.stats.active.Store(1)
	var flows sync.Map
	client := netip.MustParseAddrPort("127.0.0.1:12345")
	flows.Store(client, flow)
	f.pumpUDPReplies(&flows, flow, nil, client)
	if len(backend.deadlines) != 1 || f.Stats().Active != 0 || member.downUntil.Load() != 0 {
		t.Fatal("idle timeout was renewed, retained a flow or marked the backend unhealthy")
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
