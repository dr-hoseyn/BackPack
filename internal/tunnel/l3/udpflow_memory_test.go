//go:build linux

package l3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func udpBenchmarkProcessCPU(b *testing.B) time.Duration {
	b.Helper()
	var r unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &r); err != nil {
		b.Fatal(err)
	}
	return time.Duration(r.Utime.Sec+r.Stime.Sec)*time.Second + time.Duration(r.Utime.Usec+r.Stime.Usec)*time.Microsecond
}

// This benchmark also compiles against the previous implementation so CI can
// compare retained Go heap on actual idle backend reply workers. It does not
// measure RSS, kernel socket memory, WAN capacity or packet throughput.
func BenchmarkUDPIdleReplyFlows(b *testing.B) {
	const count = 300
	var retained uint64
	for range b.N {
		backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			b.Fatal(err)
		}
		local, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			backend.Close()
			b.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		var flows sync.Map
		var workers sync.WaitGroup
		log := logrus.New()
		log.SetOutput(io.Discard)
		f := &Forwarder{log: log}
		pool := newBackendPool([]string{backend.LocalAddr().String()})
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		var flowErr error
		for i := range count {
			client := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(10000+i))
			if _, flowErr = f.udpFlowFor(ctx, &flows, local, client, pool, nil, &workers); flowErr != nil {
				break
			}
		}
		// Let all newly started workers reach their socket wait before taking
		// the heap sample. There is no assertion on wall-clock scheduling.
		time.Sleep(50 * time.Millisecond)
		runtime.GC()
		runtime.ReadMemStats(&after)
		if after.HeapAlloc > before.HeapAlloc {
			retained += after.HeapAlloc - before.HeapAlloc
		}
		cancel()
		flows.Range(func(_, value any) bool { value.(*udpFlow).close(); return true })
		workers.Wait()
		local.Close()
		backend.Close()
		if flowErr != nil {
			b.Fatal(flowErr)
		}
		if f.Stats().Active != 0 || pool.members[0].active.Load() != 0 {
			b.Fatal("idle flow cleanup retained a worker or backend slot")
		}
	}
	b.ReportMetric(float64(retained)/float64(b.N*count), "heap-B/idle-flow")
}

// Verify a real backend reply through the flow's normal pump on every turn,
// including the final local UDP write. This exposes the readiness syscall's
// hot-path cost alongside the idle-memory measurement.
func BenchmarkUDPForwardedReplies(b *testing.B) {
	for _, size := range []int{64, 1200, 60000} {
		b.Run(fmt.Sprintf("bytes-%d", size), func(b *testing.B) {
			listen := func() *net.UDPConn {
				c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { c.Close() })
				return c
			}
			backend, local, client := listen(), listen(), listen()
			ctx, cancel := context.WithCancel(context.Background())
			var workers sync.WaitGroup
			var flows sync.Map
			log := logrus.New()
			log.SetOutput(io.Discard)
			f := &Forwarder{log: log}
			pool := newBackendPool([]string{backend.LocalAddr().String()})
			flow, err := f.udpFlowFor(ctx, &flows, local, client.LocalAddr().(*net.UDPAddr).AddrPort(), pool, nil, &workers)
			if err != nil {
				cancel()
				b.Fatal(err)
			}
			defer func() {
				cancel()
				flow.close()
				workers.Wait()
			}()
			target := flow.socket.LocalAddr().(*net.UDPAddr)
			want := bytes.Repeat([]byte("x"), size)
			buf := make([]byte, udpBufferSize)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			cpuStart := udpBenchmarkProcessCPU(b)
			for range b.N {
				_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
				if _, err := backend.WriteToUDP(want, target); err != nil {
					b.Fatal(err)
				}
				n, _, err := client.ReadFromUDP(buf)
				if err != nil || !bytes.Equal(buf[:n], want) {
					b.Fatalf("forwarded reply changed: bytes=%d err=%v", n, err)
				}
			}
			cpuElapsed := udpBenchmarkProcessCPU(b) - cpuStart
			b.StopTimer()
			// Includes this benchmark process's client, backend and reply
			// pump; it is not an isolated production server CPU measurement.
			b.ReportMetric(float64(cpuElapsed)/float64(b.N), "cpu-ns/op")
		})
	}
}
