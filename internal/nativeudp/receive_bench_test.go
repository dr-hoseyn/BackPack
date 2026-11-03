package nativeudp

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/flynn/noise"
)

// Supplies real authenticated frames synchronously, without socket allocation,
// handshake timers or a WAN. Include sender nonce cost in every before/after
// measurement; the receiver's scratch ownership is the behavior under test.
type cipherPacketSource struct {
	benchmarkSocket
	cipher    noise.Cipher
	from      net.Addr
	id        [16]byte
	body      []byte
	plain     []byte
	remaining int
	message   uint64
	counter   uint64
	part      int
}

func (s *cipherPacketSource) ReadFrom(b []byte) (int, net.Addr, error) {
	if s.remaining == 0 {
		return 0, nil, net.ErrClosed
	}
	if s.part == 0 && len(s.body) > 0 {
		s.body[0] = byte(s.message)
	}
	parts := max(1, (len(s.body)+fragmentSize-1)/fragmentSize)
	if parts == 1 {
		s.plain = append(s.plain[:0], 2)
		s.plain = append(s.plain, s.body...)
	} else {
		s.plain = s.plain[:fragmentHeader]
		s.plain[0] = 3
		binary.BigEndian.PutUint64(s.plain[1:9], s.message)
		binary.BigEndian.PutUint16(s.plain[9:11], uint16(s.part))
		binary.BigEndian.PutUint16(s.plain[11:13], uint16(parts))
		binary.BigEndian.PutUint32(s.plain[13:17], uint32(len(s.body)))
		offset := s.part * fragmentSize
		s.plain = append(s.plain, s.body[offset:min(offset+fragmentSize, len(s.body))]...)
	}
	b[0] = 3
	copy(b[1:17], s.id[:])
	binary.BigEndian.PutUint64(b[17:25], s.counter)
	wire := s.cipher.Encrypt(b[:headerSize], s.counter, b[:headerSize], s.plain)
	s.counter++
	s.part++
	if s.part == parts {
		s.part = 0
		s.message++
		s.remaining--
	}
	return len(wire), s.from, nil
}

func receivingLink(t testing.TB, size, count int, dial bool) *Link {
	t.Helper()
	key := [32]byte{0x73}
	id := [16]byte{0x41}
	from := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}
	source := &cipherPacketSource{
		cipher: suite.Cipher(key), from: from, id: id,
		body: make([]byte, size), plain: make([]byte, 0, fragmentHeader+fragmentSize),
		remaining: count, message: 1,
	}
	for i := range source.body {
		source.body[i] = byte(i)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	l := &Link{
		ctx: ctx, cancel: cancel, pc: source, peer: from, dial: dial,
		active:     &keys{id: id, created: time.Now(), recv: suite.Cipher(key)},
		assemblies: make(map[fragmentKey]*assembly), in: make(chan []byte, 128),
	}
	l.wg.Add(1)
	return l
}

func TestNativeReceiveScratchNeverOverwritesQueuedMessages(t *testing.T) {
	for _, size := range []int{0, 64, 1200, maxPayload} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			l := receivingLink(t, size, 3, false)
			l.read() // All messages remain queued while subsequent frames reuse scratch.
			for message := 1; message <= 3; message++ {
				select {
				case got := <-l.in:
					if len(got) != size {
						t.Fatalf("message %d: size=%d, want %d", message, len(got), size)
					}
					for i, value := range got {
						want := byte(i)
						if i == 0 {
							want = byte(message)
						}
						if value != want {
							t.Fatalf("message %d byte %d overwritten: got=%d want=%d", message, i, value, want)
						}
					}
				default:
					t.Fatalf("message %d missing, including an empty message", message)
				}
			}
		})
	}
}

func TestNativeUDPClientStillRequiresItsConfiguredPacketPeer(t *testing.T) {
	for _, tc := range []struct {
		name       string
		from, peer net.Addr
		accepted   bool
	}{
		{"IPv4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}, &net.UDPAddr{IP: net.IP{127, 0, 0, 1}, Port: 12345}, true},
		{"wrong-ip", &net.UDPAddr{IP: net.ParseIP("127.0.0.2"), Port: 12345}, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}, false},
		{"wrong-port", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12346}, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}, false},
		{"IPv6-zone", &net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0", Port: 12345}, &net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0", Port: 12345}, true},
		{"wrong-zone", &net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth1", Port: 12345}, &net.UDPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0", Port: 12345}, false},
		{"wrapped-address", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := receivingLink(t, 64, 1, true)
			l.pc.(*cipherPacketSource).from = tc.from
			l.peer = tc.peer
			l.read()
			if got := l.received.Load() == 1; got != tc.accepted {
				t.Fatalf("packet peer accepted=%v, want %v", got, tc.accepted)
			}
		})
	}
}

func BenchmarkNativeUDPAuthenticatedReceive(b *testing.B) {
	for _, dial := range []bool{false, true} {
		for _, size := range []int{64, 1200, maxPayload} {
			b.Run(fmt.Sprintf("dial=%v/bytes=%d", dial, size), func(b *testing.B) {
				l := receivingLink(b, size, b.N, dial)
				drained := make(chan struct{})
				go func() {
					defer close(drained)
					for range l.in {
					}
				}()
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				l.read()
				close(l.in)
				<-drained
				b.StopTimer()
				if got := l.received.Load() + l.dropped.Load(); got != uint64(b.N) {
					b.Fatalf("processed %d messages, want %d", got, b.N)
				}
			})
		}
	}
}
