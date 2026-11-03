package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/xtaci/smux"
)

func muxRelaySource(t testing.TB, version int) (send, receive *smux.Stream) {
	t.Helper()
	a, b := net.Pipe()
	cfg := smux.DefaultConfig()
	cfg.Version = version
	cfg.MaxReceiveBuffer, cfg.MaxStreamBuffer = 8<<20, 4<<20
	server, err := smux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err := smux.Client(b, cfg)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	send, err = client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	receive, err = server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	return send, receive
}

func muxTCPDestination(t testing.TB) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peer, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	destination, err := ln.Accept()
	if err != nil {
		peer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close(); destination.Close() })
	return destination, peer
}

func TestMuxReceiveRelayPreservesPayloadAndTunnelAccounting(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			send, receive := muxRelaySource(t, version)
			destination, peer := muxTCPDestination(t)
			want := bytes.Repeat([]byte("mux-copy-payload"), 16000)
			inBefore, outBefore := metrics.Traffic()
			done := make(chan struct{})
			go func() {
				defer close(done)
				transferData(metrics.CountedConn(receive), destination, quietLogger(), nil, 0, false)
			}()
			written := make(chan error, 1)
			go func() {
				_, err := send.Write(want)
				written <- err
				send.Close()
			}()
			_ = peer.SetReadDeadline(time.Now().Add(3 * time.Second))
			got, err := io.ReadAll(peer)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("version %d relay received %d/%d bytes: %v", version, len(got), len(want), err)
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("mux receive relay retained its worker after EOF")
			}
			inAfter, outAfter := metrics.Traffic()
			if inAfter-inBefore != uint64(len(want)) || outAfter != outBefore {
				t.Fatalf("mux receive accounting in=%d out=%d", inAfter-inBefore, outAfter-outBefore)
			}
		})
	}
}

func TestMuxReceiveFastCopyDoesNotUnwrapPacingOrUnknownWriters(t *testing.T) {
	_, receive := muxRelaySource(t, 2)
	destination, _ := muxTCPDestination(t)
	// An opaque wrapper can pace bytes or hold failed writes. It must continue
	// receiving owned relay buffers rather than borrowed smux receive slices.
	wrappedSource := struct{ net.Conn }{receive}
	wrappedDestination := struct{ net.Conn }{destination}
	if transferMuxToTCP(wrappedSource, destination, quietLogger(), nil, 0, false) ||
		transferMuxToTCP(receive, wrappedDestination, quietLogger(), nil, 0, false) {
		t.Fatal("direct mux receive copy bypassed an unknown connection wrapper")
	}
}

func BenchmarkMuxToTCPRelay(b *testing.B) {
	for _, version := range []int{1, 2} {
		b.Run(fmt.Sprint(version), func(b *testing.B) {
			send, receive := muxRelaySource(b, version)
			destination, peer := muxTCPDestination(b)
			body := make([]byte, 64<<10)
			done := make(chan struct{})
			go func() {
				defer close(done)
				transferData(metrics.CountedConn(receive), destination, quietLogger(), nil, 0, false)
			}()
			type result struct {
				n   int64
				err error
			}
			drained := make(chan result, 1)
			go func() { n, err := io.Copy(io.Discard, peer); drained <- result{n, err} }()
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := send.Write(body); err != nil {
					b.Fatal(err)
				}
			}
			send.Close()
			<-done
			got := <-drained
			b.StopTimer()
			if got.err != nil || got.n != int64(b.N)*int64(len(body)) {
				b.Fatalf("drained %d bytes: %v", got.n, got.err)
			}
		})
	}
}
