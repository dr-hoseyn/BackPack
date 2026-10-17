package masqueh2

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	s := httptest.NewTLSServer(nil)
	defer s.Close()
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return &tls.Config{Certificates: s.TLS.Certificates}, &tls.Config{RootCAs: pool, ServerName: "example.com"}
}

func TestHTTP2CONNECTUDPTransfersConcurrentCapsulesBeyondTheInitialFlowWindow(t *testing.T) {
	serverTLS, clientTLS := testTLS(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Listen(ctx, "127.0.0.1:0", "127.0.0.1:62101", "a-long-private-token-for-this-test", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := Dial(ctx, s.LocalAddr().String(), "127.0.0.1:62101", "a-long-private-token-for-this-test", clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Constrain send credits to HTTP/2's original 64 KiB window so this test
	// must wait for WINDOW_UPDATE even with a bounded application pipeline.
	for _, pc := range []*Conn{s, c} {
		pc.mu.Lock()
		for _, stream := range pc.sessions {
			stream.mu.Lock()
			stream.connWindow = min(stream.connWindow, 65535)
			stream.streamWindow = min(stream.streamWindow, 65535)
			stream.mu.Unlock()
		}
		pc.mu.Unlock()
	}
	deadline := time.Now().Add(20 * time.Second)
	s.SetDeadline(deadline)
	c.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() {
		b := make([]byte, maxPacket)
		for range 128 {
			n, a, e := s.ReadFrom(b)
			if e != nil {
				done <- e
				return
			}
			if _, e := s.WriteTo(b[:n], a); e != nil {
				done <- e
				return
			}
		}
		done <- nil
	}()
	var writers sync.WaitGroup
	errs := make(chan error, 4)
	// PacketConn deliberately drops at its bounded receive queue. Keep the
	// outstanding test workload below that budget while still exercising four
	// writers and several complete flow-control windows of bidirectional data.
	inFlight := make(chan struct{}, 32)
	for i := range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := range 32 {
				select {
				case inFlight <- struct{}{}:
				case <-ctx.Done():
					return
				}
				p := bytes.Repeat([]byte{byte(i), byte(j)}, 12000)
				if _, err := c.WriteTo(p, c.Peer()); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	seen := map[[2]byte]bool{}
	b := make([]byte, maxPacket)
	for range 128 {
		n, _, err := c.ReadFrom(b)
		if err != nil {
			t.Fatalf("received %d/128 packets; receive-queue drops server=%d client=%d: %v", len(seen), s.dropped.Load(), c.dropped.Load(), err)
		}
		if n != 24000 || !bytes.Equal(b[:n], bytes.Repeat(b[:2], 12000)) {
			t.Fatal("datagram boundaries or payload corrupted")
		}
		id := [2]byte{b[0], b[1]}
		if seen[id] {
			t.Fatal("duplicate datagram")
		}
		seen[id] = true
		<-inFlight
	}
	writers.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestHTTP2ReceiveQueueDropsExcessDatagramsInsteadOfGrowingOrBlockingControl(t *testing.T) {
	serverTLS, clientTLS := testTLS(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s, err := Listen(ctx, "127.0.0.1:0", "127.0.0.1:62101", "private-bounded-queue-token", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := Dial(ctx, s.LocalAddr().String(), "127.0.0.1:62101", "private-bounded-queue-token", clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	for range 256 {
		if _, err := c.WriteTo(bytes.Repeat([]byte{9}, 1000), c.Peer()); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for uint64(len(s.in))+s.dropped.Load() < 256 {
		if time.Now().After(deadline) {
			t.Fatal("receive overload blocked HTTP2 processing")
		}
		time.Sleep(time.Millisecond)
	}
	if len(s.in) != 64 || s.dropped.Load() != 192 {
		t.Fatalf("unbounded or unreported receive queue: %d packets, %d drops", len(s.in), s.dropped.Load())
	}
}

func TestHTTP2CONNECTUDPRejectsBadCredentialsWrongTargetsAndUntrustedTLS(t *testing.T) {
	st, ct := testTLS(t)
	s, err := Listen(t.Context(), "127.0.0.1:0", "127.0.0.1:62101", "a-long-private-token-for-this-test", st)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, test := range []struct {
		target, token string
		tls           *tls.Config
	}{
		{"127.0.0.1:62101", "a-different-private-token-for-test", ct},
		{"127.0.0.1:62102", "a-long-private-token-for-this-test", ct},
		{"127.0.0.1:62101", "a-long-private-token-for-this-test", &tls.Config{ServerName: "example.com"}},
		{"203.0.113.1:53", "a-long-private-token-for-this-test", ct},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		c, err := Dial(ctx, s.LocalAddr().String(), test.target, test.token, test.tls)
		cancel()
		if err == nil {
			c.Close()
			t.Fatal("accepted invalid peer or destination")
		}
	}
}

func TestHTTP2CONNECTUDPCancellationUnblocksReadsAndReleasesListener(t *testing.T) {
	st, ct := testTLS(t)
	ctx, cancel := context.WithCancel(t.Context())
	s, err := Listen(ctx, "127.0.0.1:0", "127.0.0.1:62101", "a-long-private-token-for-this-test", st)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Dial(ctx, s.LocalAddr().String(), "127.0.0.1:62101", "a-long-private-token-for-this-test", ct)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, _, err := c.ReadFrom(make([]byte, 100)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read deadline: %v", err)
	}
	c.SetReadDeadline(time.Time{})
	done := make(chan error, 1)
	go func() { _, _, e := c.ReadFrom(make([]byte, 100)); done <- e }()
	addr := s.LocalAddr().String()
	cancel()
	s.Close()
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read survived cancellation")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal("listener survived close:", err)
	}
	l.Close()
}

func TestCapsulesSurviveEveryBoundaryAndSkipUnknownTypesWithoutAllocation(t *testing.T) {
	p := bytes.Repeat([]byte{1, 2, 3}, 500)
	wire := datagramCapsule(p)
	for split := range len(wire) + 1 {
		var d decoder
		var got []byte
		deliver := func(b []byte) { got = append(got, b...) }
		if err := d.feed(wire[:split], deliver); err != nil {
			t.Fatal(err)
		}
		if err := d.feed(wire[split:], deliver); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, p) {
			t.Fatalf("split %d corrupted packet", split)
		}
	}
	var d decoder
	header := appendVarint(appendVarint(nil, 99), 1<<30)
	if err := d.feed(header, func([]byte) { t.Fatal("delivered unknown capsule") }); err != nil {
		t.Fatal(err)
	}
	if d.skip != 1<<30 || len(d.buf) != 0 {
		t.Fatal("unknown capsule was buffered")
	}
	bad := appendVarint(appendVarint(nil, 0), maxPacket+100)
	if err := (&decoder{}).feed(bad, func([]byte) {}); err == nil {
		t.Fatal("oversized datagram accepted")
	}
}
