//go:build linux

package network

import (
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/sys/unix"
)

func TestTCPFWDNUM3QueueLimitPreservesTheSocketSendBuffer(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		accepted <- conn
	}()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer := <-accepted
	defer peer.Close()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var before, after, limit int
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		before, optionErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
	}); err != nil || optionErr != nil {
		t.Fatalf("read socket buffer: %v, %v", err, optionErr)
	}
	limitTCPMuxBacklog(conn)
	if err := raw.Control(func(fd uintptr) {
		limit, optionErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT)
		after, _ = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
	}); err != nil {
		t.Fatal(err)
	}
	if errors.Is(optionErr, unix.ENOPROTOOPT) || errors.Is(optionErr, unix.EOPNOTSUPP) {
		t.Skip("kernel does not support TCP_NOTSENT_LOWAT")
	}
	if optionErr != nil || limit != 512<<10 {
		t.Fatalf("unsent queue limit = %d, %v", limit, optionErr)
	}
	if after < before {
		t.Fatalf("queue tuning shrank the send buffer: %d -> %d", before, after)
	}
}

func TestTCPMuxPressureAccountsForQueuedDataAndRTTInflation(t *testing.T) {
	for _, tc := range []struct {
		name string
		info unix.TCPInfo
		want time.Duration
	}{
		{"idle", unix.TCPInfo{}, 0},
		{"unacknowledged stream open after RTO", unix.TCPInfo{Unacked: 1, Retransmits: 1, Rtt: 100_000, Min_rtt: 100_000}, time.Second},
		{"recovered retransmission", unix.TCPInfo{Retransmits: 1}, 0},
		{"queued data", unix.TCPInfo{Notsent_bytes: 100_000, Delivery_rate: 1_000_000}, 100 * time.Millisecond},
		{"stale delayed ACK", unix.TCPInfo{Rtt: 80_000, Min_rtt: 30_000}, 0},
		{"slower acknowledgements with a backlog", unix.TCPInfo{Rtt: 80_000, Min_rtt: 30_000, Notsent_bytes: 10_000, Delivery_rate: 1_000_000}, 60 * time.Millisecond},
		{"slow measured link", unix.TCPInfo{Rtt: 100_000, Min_rtt: 100_000, Snd_cwnd: 1000, Snd_mss: 1000, Notsent_bytes: 900_000, Delivery_rate: 1_000_000}, 900 * time.Millisecond},
		{"shrinking receiver window", unix.TCPInfo{Rtt: 100_000, Min_rtt: 100_000, Snd_wnd: 10_000, Notsent_bytes: 20_000, Delivery_rate: 1_000_000}, 200 * time.Millisecond},
		{"closed receiver window", unix.TCPInfo{Probes: 1, Rtt: 100_000, Min_rtt: 100_000, Notsent_bytes: 20_000, Delivery_rate: 1_000_000}, time.Second},
		{"window estimate", unix.TCPInfo{Rtt: 100_000, Min_rtt: 100_000, Snd_cwnd: 10, Snd_mss: 1000, Notsent_bytes: 20_000}, 200 * time.Millisecond},
		{"bounded slow socket", unix.TCPInfo{Notsent_bytes: ^uint32(0), Delivery_rate: 1}, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tcpMuxQueueDelay(&tc.info); got != tc.want {
				t.Fatalf("queue delay = %s, want %s", got, tc.want)
			}
		})
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if pressure := TCPMuxPressure(a)(); pressure != 0 {
		t.Fatalf("a connection without TCP information had pressure %d", pressure)
	}
}

func TestTCPMuxPressureQueueReserveScalesWithinBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		info unix.TCPInfo
		want int
	}{
		{"unknown path", unix.TCPInfo{}, 512 << 10},
		{"low bandwidth", unix.TCPInfo{Pacing_rate: 1_000_000, Min_rtt: 100_000}, 512 << 10},
		{"wide high RTT pipe", unix.TCPInfo{Pacing_rate: 80_000_000, Min_rtt: 100_000}, 1 << 20},
		{"bounded fast path", unix.TCPInfo{Pacing_rate: 1_000_000_000, Min_rtt: 100_000}, 2 << 20},
		{"unavailable pacing rate", unix.TCPInfo{Pacing_rate: ^uint64(0), Min_rtt: 100_000}, 512 << 10},
		{"unavailable minimum RTT", unix.TCPInfo{Pacing_rate: 80_000_000, Min_rtt: ^uint32(0)}, 512 << 10},
		{"large counters", unix.TCPInfo{Pacing_rate: ^uint64(0) - 1, Min_rtt: ^uint32(0) - 1}, 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tcpMuxQueueReserve(&tc.info); got != tc.want {
				t.Fatalf("queue reserve = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestTCPMuxPressureKeepsWarmCapacityThroughOrdinaryRTTJitter(t *testing.T) {
	info := unix.TCPInfo{Rtt: 100_000, Min_rtt: 100_000, Delivery_rate: 1_000_000}
	info.Notsent_bytes = 50_000
	if got := tcpMuxQueuePressure(&info); got >= 16 {
		t.Fatalf("one ordinary RTT sent work onto a cold spare: %d", got)
	}
	info.Notsent_bytes = 900_000
	if got := tcpMuxQueuePressure(&info); got <= 16 {
		t.Fatalf("sustained backlog could not yield to a spare: %d", got)
	}
	info.Rtt, info.Min_rtt = 500_000, 500_000
	if got := tcpMuxQueuePressure(&info); got <= 16 {
		t.Fatalf("a high-RTT carrier could never yield to a spare: %d", got)
	}
}

func TestWSMuxTCPSnapshotFindsTheSocketUnderClientUpgradeAndTLS(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	tlsConn := utls.UClient(conn, &utls.Config{}, utls.HelloChrome_133)
	// As on the dialer side, the upgrade wrapper sits outside the TLS socket.
	upgrade := muxUpgradeStream(tlsConn).(*muxUpgradeConn)
	defer upgrade.releaseReader()
	if sample := SampleMuxTCP(upgrade); !sample.Available || sample.RTTUS == 0 {
		t.Fatalf("missing TCP snapshot through client wrappers: %+v", sample)
	}
}

func BenchmarkMuxPressureSampling(b *testing.B) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	peer, err := listener.Accept()
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()

	b.Run("TCP_INFO", func(b *testing.B) {
		pressure := muxPressure(conn, 0)
		b.ReportAllocs()
		for b.Loop() {
			pressure()
		}
	})
	b.Run("Cached", func(b *testing.B) {
		pressure := muxPressure(conn, time.Hour)
		b.ReportAllocs()
		for b.Loop() {
			pressure()
		}
	})
}

type countedMuxSocket struct {
	*net.TCPConn
	handles int
}

func (c *countedMuxSocket) SyscallConn() (syscall.RawConn, error) {
	c.handles++
	return c.TCPConn.SyscallConn()
}

func TestWSMuxPressureReusesTheTCPHandleAndToleratesClose(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	socket := &countedMuxSocket{TCPConn: conn}
	pressure := muxPressure(socket, 0)
	var samples sync.WaitGroup
	for range 8 {
		samples.Go(func() {
			for range 32 {
				pressure()
			}
		})
	}
	_ = conn.Close()
	samples.Wait()
	if got := pressure(); got != 0 {
		t.Fatalf("closed socket still reports pressure %d", got)
	}
	if socket.handles != 1 {
		t.Fatalf("created %d raw handles, want one per carrier", socket.handles)
	}
}
