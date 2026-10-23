package nativeudp

import (
	"bytes"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Delay the actual Noise response until the first fragment is on the wire,
// then let the sender install the new key before writing the next fragment.
// Both peers still authenticate and reassemble through the real read loops.
type rekeyGateSocket struct {
	net.PacketConn
	mu       sync.Mutex
	hold     bool
	response []byte
	to       net.Addr
	captured chan struct{}
	after    func() error
}

func (s *rekeyGateSocket) WriteTo(b []byte, to net.Addr) (int, error) {
	s.mu.Lock()
	if s.hold && b[0] == 2 {
		s.hold = false
		s.response, s.to = bytes.Clone(b), to
		close(s.captured)
		s.mu.Unlock()
		return len(b), nil
	}
	var after func() error
	if b[0] == 3 && len(b) > 100 {
		after, s.after = s.after, nil
	}
	s.mu.Unlock()
	n, err := s.PacketConn.WriteTo(b, to)
	if err == nil && after != nil {
		err = after()
	}
	return n, err
}

func TestNativeRekeyDeliversWholeDatagramsThroughThePreviousSession(t *testing.T) {
	for _, size := range []int{1001, 3000, maxPayload} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			listen := func() *rekeyGateSocket {
				pc, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { pc.Close() })
				return &rekeyGateSocket{PacketConn: pc}
			}
			serverSocket, clientSocket := listen(), listen()
			server, err := NewLink(t.Context(), serverSocket, nil, "real-rekey-delivery-test-token")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close() })
			client, err := NewLink(t.Context(), clientSocket, serverSocket.LocalAddr(), "real-rekey-delivery-test-token")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			wait := func(ready func() bool) bool {
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					if ready() {
						return true
					}
					time.Sleep(time.Millisecond)
				}
				return false
			}
			if !wait(func() bool { return server.Stats().Connected && client.Stats().Connected }) {
				t.Fatal("initial Noise handshake did not finish")
			}
			serverSocket.mu.Lock()
			serverSocket.hold = true
			serverSocket.captured = make(chan struct{})
			captured := serverSocket.captured
			serverSocket.mu.Unlock()
			client.mu.Lock()
			old := client.active.id
			err = client.begin(time.Now())
			var handshake []byte
			if err == nil {
				handshake = bytes.Clone(client.attempt.packet)
			}
			client.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := client.rawWrite(handshake, serverSocket.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-captured:
			case <-time.After(3 * time.Second):
				t.Fatal("second Noise response was not captured")
			}
			clientSocket.mu.Lock()
			clientSocket.after = func() error {
				serverSocket.mu.Lock()
				packet, to := serverSocket.response, serverSocket.to
				serverSocket.mu.Unlock()
				if _, err := serverSocket.PacketConn.WriteTo(packet, to); err != nil {
					return err
				}
				if !wait(func() bool {
					client.mu.Lock()
					defer client.mu.Unlock()
					return client.active.id != old
				}) {
					return errNotReady
				}
				return nil
			}
			clientSocket.mu.Unlock()
			want := make([]byte, size)
			for i := range want {
				want[i] = byte(i * 37)
			}
			if err := client.Write(want); err != nil {
				t.Fatal(err)
			}
			// A following message must use the new session and arrive as a
			// separate message, without waiting for the fragmented one.
			next := []byte("the next Noise session is usable")
			if err := client.Write(next); err != nil {
				t.Fatal(err)
			}
			got := make(chan []byte, 2)
			go func() {
				buf := make([]byte, maxPayload)
				for range 2 {
					n, err := server.Read(buf)
					if err != nil {
						return
					}
					got <- bytes.Clone(buf[:n])
				}
			}()
			seen := [2]bool{}
			for range 2 {
				select {
				case body := <-got:
					switch {
					case bytes.Equal(body, want) && !seen[0]:
						seen[0] = true
					case bytes.Equal(body, next) && !seen[1]:
						seen[1] = true
					default:
						t.Fatal("rekey delivered a duplicate or corrupted datagram")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("fragmented datagram was lost across a completed Noise rekey")
				}
			}
		})
	}
}
