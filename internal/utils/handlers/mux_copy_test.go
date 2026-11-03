package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/metrics"
	"github.com/backpack/backpack/internal/traffic"
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
			usageCtx, stopUsage := context.WithCancel(t.Context())
			defer stopUsage()
			usageFile := filepath.Join(t.TempDir(), "traffic.json")
			usage := traffic.NewDataStore(usageCtx, usageFile, true, quietLogger())
			inBefore, outBefore := metrics.Traffic()
			done := make(chan struct{})
			go func() {
				defer close(done)
				transferData(metrics.CountedConn(receive), destination, quietLogger(), usage, 8080, true)
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
			stopUsage()
			usage.Run() // Flush the same per-port counters the sniffer persists.
			data, err := os.ReadFile(usageFile)
			if err != nil {
				t.Fatal(err)
			}
			var ports []traffic.PortUsage
			if err := json.Unmarshal(data, &ports); err != nil || len(ports) != 1 || ports[0].Port != 8080 || ports[0].Usage != uint64(len(want)) {
				t.Fatalf("mux receive lost per-port accounting: %s %v", data, err)
			}
		})
	}
}

func TestMuxReceiveRelayCancellationJoinsBothDirections(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			send, receive := muxRelaySource(t, version)
			destination, _ := muxTCPDestination(t) // Peer deliberately never drains.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				TCPConnectionHandler(ctx, false, metrics.CountedConn(receive), destination, quietLogger(), nil, 0, false)
			}()
			writing := make(chan struct{})
			go func() {
				defer close(writing)
				body := make([]byte, 64<<10)
				for {
					if _, err := send.Write(body); err != nil {
						return
					}
				}
			}()
			time.Sleep(20 * time.Millisecond)
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancelled mux receive relay retained a copy direction")
			}
			send.Close()
			select {
			case <-writing:
			case <-time.After(time.Second):
				t.Fatal("mux sender remained blocked after cancellation")
			}
		})
	}
}

// v2 starts with a 256 KiB peer window. Read advertises the configured window
// before writing to the backend; WriterTo delays that update until Write
// returns. If the backend is congested, that needlessly stalls the mux sender
// even though the session still has receive capacity for the stream.
func TestMuxReceiveWindowCreditDoesNotWaitForBlockedTCP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux TCP send/receive buffer backpressure semantics")
	}
	send, receive := muxRelaySource(t, 2)
	destination, peer := muxTCPDestination(t)
	if err := destination.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
		t.Fatal(err)
	}
	if err := peer.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	// Fill the socket without a peer reader, then clear the timeout. All later
	// writes must wait for cancellation rather than reaching the TCP peer.
	_ = destination.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	n, err := destination.Write(make([]byte, 4<<20))
	var timeout net.Error
	if n == 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("could not congest TCP destination: wrote %d bytes, error %v", n, err)
	}
	_ = destination.SetWriteDeadline(time.Time{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		TCPConnectionHandler(ctx, false, metrics.CountedConn(receive), destination, quietLogger(), nil, 0, false)
	}()
	written := make(chan error, 1)
	go func() { _, err := send.Write(make([]byte, 1<<20)); written <- err }()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked TCP write withheld mux receive credit despite available stream capacity")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("congested relay did not join its workers after cancellation")
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
