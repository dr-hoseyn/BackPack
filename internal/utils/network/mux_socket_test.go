package network

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

type shortMuxVectorConn struct{ net.Conn }

func (c shortMuxVectorConn) WriteBuffers(parts [][]byte) (int, error) {
	// Simulate a carrier that reports a truncated header as successful.
	return len(parts[0]) - 1, nil
}

func TestWatchedMuxRetiresAfterAShortVectorWrite(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	cfg := smux.DefaultConfig()
	cfg.KeepAliveDisabled = true
	session, err := NewWatchedMuxSession(shortMuxVectorConn{local}, cfg, smux.Client)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	// The sender's write error and session retirement can race to wake
	// OpenStream. Either way, it must not publish a stream.
	if _, err := session.OpenStream(); !errors.Is(err, io.ErrShortWrite) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("open with a truncated mux header returned %v", err)
	}
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("short vector write left the session in the pool")
	}
}

func muxTCPPair(t testing.TB) (*net.TCPConn, net.Conn) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	p, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return c.(*net.TCPConn), p
}

func TestWatchedMuxTCPVectorPreservesFrames(t *testing.T) {
	c, peer := muxTCPPair(t)
	c.SetDeadline(time.Now().Add(5 * time.Second))
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	var socket io.ReadWriteCloser
	cfg := smux.DefaultConfig()
	cfg.KeepAliveDisabled = true
	session, err := NewWatchedMuxSession(c, cfg, func(w io.ReadWriteCloser, cfg *smux.Config) (*smux.Session, error) {
		socket = w
		return smux.Client(w, cfg)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	w, ok := socket.(*muxTCPSocket)
	if !ok {
		t.Fatal("TCP session does not use vectored writes")
	}
	// No stream has opened, so smux's sender is idle during these writes.
	header, payload := []byte("header!!"), bytes.Repeat([]byte("x"), 65535)
	parts := [][]byte{header, nil, payload}
	want := append(append([]byte(nil), header...), payload...)
	done := make(chan error, 1)
	go func() {
		for range 3 {
			got := make([]byte, len(want))
			if _, err := io.ReadFull(peer, got); err != nil {
				done <- err
				return
			}
			if !bytes.Equal(got, want) {
				done <- io.ErrUnexpectedEOF
				return
			}
		}
		done <- nil
	}()
	for range 3 {
		n, err := w.WriteBuffers(parts)
		if err != nil || n != len(want) {
			t.Fatalf("write = %d, %v", n, err)
		}
		if len(parts[0]) != len(header) || len(parts[2]) != len(payload) {
			t.Fatal("caller descriptors were consumed")
		}
		for _, p := range w.buffers[:cap(w.buffers)] {
			if p != nil {
				t.Fatal("socket retains payload after write")
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWatchedMuxTCPRoundTrip(t *testing.T) {
	for _, version := range []int{1, 2} {
		c, peer := muxTCPPair(t)
		cfg := smux.DefaultConfig()
		cfg.Version = version
		client, err := NewWatchedMuxSession(c, cfg, smux.Client)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		server, err := NewWatchedMuxSession(peer, cfg, smux.Server)
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		done := make(chan error, 1)
		go func() {
			s, err := server.AcceptStream()
			if err != nil {
				done <- err
				return
			}
			s.SetDeadline(time.Now().Add(5 * time.Second))
			_, err = io.Copy(s, s)
			s.Close()
			done <- err
		}()
		s, err := client.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		s.SetDeadline(time.Now().Add(5 * time.Second))
		want := bytes.Repeat([]byte("tcp-mux-round-trip"), 65536)
		written := make(chan error, 1)
		go func() { _, err := s.Write(want); written <- err }()
		got := make([]byte, len(want))
		if _, err := io.ReadFull(s, got); err != nil {
			t.Fatal(err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("version %d corrupted payload", version)
		}
		s.Close()
		// smux's WriterTo reports EOF when the peer closes the stream.
		if err := <-done; err != nil && err != io.EOF {
			t.Fatal(err)
		}
	}
}

func BenchmarkMuxTCPFrame(b *testing.B) {
	for _, vector := range []bool{false, true} {
		name := "copied"
		if vector {
			name = "vector"
		}
		b.Run(name, func(b *testing.B) {
			c, peer := muxTCPPair(b)
			done := make(chan struct{})
			go func() { io.Copy(io.Discard, peer); close(done) }()
			defer func() { c.Close(); <-done }()
			socket := &muxSocket{Conn: c, timeout: 40 * time.Second}
			w := &muxTCPSocket{muxSocket: socket, tcp: c, buffers: make(net.Buffers, 0, 2)}
			parts := [][]byte{make([]byte, 8), make([]byte, 32*1024)}
			staging := make([]byte, 65536+8)
			b.SetBytes(int64(len(parts[1])))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				if vector {
					_, err = w.WriteBuffers(parts)
				} else {
					copy(staging, parts[0])
					copy(staging[8:], parts[1])
					_, err = socket.Write(staging[:8+len(parts[1])])
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
		})
	}
}

func TestWatchedMuxRetiresAnIdleSessionOnPeerClose(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	session, err := NewWatchedMuxSession(local, nil, smux.Client)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	peer.Close()
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("idle session remained in the pool after the socket closed")
	}
}

func TestWatchedMuxInterruptsAStalledSocketWrite(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	cfg := smux.DefaultConfig()
	cfg.KeepAliveDisabled = true
	cfg.KeepAliveTimeout = 30 * time.Millisecond
	session, err := NewWatchedMuxSession(local, cfg, smux.Client)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	done := make(chan error, 1)
	go func() {
		_, err := session.OpenStream()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked stream unexpectedly opened")
		}
	case <-time.After(time.Second):
		t.Fatal("the socket write ignored the mux write limit")
	}
	select {
	case <-session.CloseChan():
	case <-time.After(time.Second):
		t.Fatal("the failed socket left its mux session live")
	}
}
