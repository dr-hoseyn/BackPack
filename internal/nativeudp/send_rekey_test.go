package nativeudp

import (
	"bytes"
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Rekey can complete while a fragmented datagram is being written. The
// receiver keys reassembly by Noise session, so every fragment must keep the
// session selected for the first fragment, even when a newer one is available.
type rekeyDuringWriteSocket struct {
	benchmarkSocket
	link *Link
	ids  [][16]byte
}

func (s *rekeyDuringWriteSocket) WriteTo(p []byte, _ net.Addr) (int, error) {
	var id [16]byte
	copy(id[:], p[1:17])
	s.ids = append(s.ids, id)
	if len(s.ids) == 1 {
		s.link.mu.Lock()
		s.link.previous = s.link.active
		s.link.active = &keys{id: [16]byte{2}, created: time.Now(), send: suite.Cipher([32]byte{2})}
		s.link.mu.Unlock()
	}
	return len(p), nil
}

func TestNativeDatagramKeepsItsSessionWhenRekeyCompletesBetweenFragments(t *testing.T) {
	link := benchmarkLink()
	link.active.id = [16]byte{1}
	socket := &rekeyDuringWriteSocket{link: link}
	link.pc = socket
	if err := link.Write(bytes.Repeat([]byte{7}, 3000)); err != nil {
		t.Fatal(err)
	}
	if len(socket.ids) != 3 {
		t.Fatalf("got %d fragments, want 3", len(socket.ids))
	}
	for _, id := range socket.ids {
		if id != [16]byte{1} {
			t.Fatal("one datagram crossed Noise sessions; the peer cannot reassemble it")
		}
	}
	if err := link.Write([]byte("next datagram")); err != nil {
		t.Fatal(err)
	}
	if socket.ids[3] != [16]byte{2} {
		t.Fatal("the next datagram did not adopt the new session")
	}
}

type engineSendSocket struct {
	benchmarkSocket
	mu      sync.Mutex
	packets [][]byte
}

func (s *engineSendSocket) WriteTo(p []byte, _ net.Addr) (int, error) {
	s.mu.Lock()
	s.packets = append(s.packets, bytes.Clone(p))
	s.mu.Unlock()
	return len(p), nil
}

func TestConcurrentNativeEngineSendsKeepTheirOwnFlowAndPayload(t *testing.T) {
	link := benchmarkLink()
	socket := &engineSendSocket{}
	link.pc = socket
	e := &Engine{link: link}
	var workers sync.WaitGroup
	for id := uint64(1); id <= 64; id++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			e.write(id, "127.0.0.1:53", bytes.Repeat([]byte{byte(id)}, 100))
		}()
	}
	workers.Wait()
	if len(socket.packets) != 64 {
		t.Fatalf("sent %d packets, want 64", len(socket.packets))
	}
	seen := make(map[uint64]bool)
	for _, packet := range socket.packets {
		counter := binary.BigEndian.Uint64(packet[17:25])
		plain, err := suite.Cipher([32]byte{1}).Decrypt(nil, counter, packet[:headerSize], packet[headerSize:])
		if err != nil || len(plain) < 11 || plain[0] != 2 {
			t.Fatalf("invalid authenticated datagram: %v", err)
		}
		id := binary.BigEndian.Uint64(plain[1:9])
		size := int(binary.BigEndian.Uint16(plain[9:11]))
		if id < 1 || id > 64 || seen[id] || string(plain[11:11+size]) != "127.0.0.1:53" ||
			!bytes.Equal(plain[11+size:], bytes.Repeat([]byte{byte(id)}, 100)) {
			t.Fatal("concurrent engine send mixed flow headers or payloads")
		}
		seen[id] = true
	}
}

func BenchmarkNativeUDPEngineSend(b *testing.B) {
	for _, size := range []int{64, 1200, 59000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			e := &Engine{link: benchmarkLink()}
			body := make([]byte, size)
			e.write(1, "127.0.0.1:53", body)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				e.write(1, "127.0.0.1:53", body)
			}
		})
	}
}

func TestNativeEngineSteadySendDoesNotAllocateAnotherPayload(t *testing.T) {
	e := &Engine{link: benchmarkLink()}
	body := make([]byte, 1200)
	for range 3 {
		e.write(1, "127.0.0.1:53", body)
	}
	if got := testing.AllocsPerRun(100, func() { e.write(1, "127.0.0.1:53", body) }); got > 2 {
		t.Fatalf("engine adds payload allocations above the two cipher nonces: %.1f", got)
	}
}
