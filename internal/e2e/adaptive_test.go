package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/metrics"
)

type controlledPath struct {
	addr    string
	delay   atomic.Int64
	blocked atomic.Bool
	bytes   atomic.Int64
}

func newControlledPath(t *testing.T, target string) *controlledPath {
	return newControlledPathOn(t, target, "tcp", "127.0.0.1:0")
}

func newControlledPathOn(t *testing.T, target, network, bind string) *controlledPath {
	t.Helper()
	l, err := net.Listen(network, bind)
	if err != nil {
		t.Fatal(err)
	}
	p := &controlledPath{addr: l.Addr().String()}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			local, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer local.Close()
				stop := context.AfterFunc(ctx, func() { local.Close() })
				defer stop()
				if p.blocked.Load() {
					return
				}
				if delay := time.Duration(p.delay.Load()); delay > 0 {
					timer := time.NewTimer(delay)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
				remote, err := net.DialTimeout("tcp", target, time.Second)
				if err != nil {
					return
				}
				defer remote.Close()
				stopRemote := context.AfterFunc(ctx, func() { remote.Close() })
				defer stopRemote()
				done := make(chan struct{})
				go func() {
					defer close(done)
					io.Copy(pathTrafficWriter{remote, &p.bytes}, local)
					remote.(*net.TCPConn).CloseWrite()
				}()
				io.Copy(local, remote)
				local.(*net.TCPConn).CloseWrite()
				local.Close()
				remote.Close()
				<-done
			}()
		}
	}()
	t.Cleanup(func() { cancel(); l.Close(); wg.Wait() })
	return p
}

type pathTrafficWriter struct {
	io.Writer
	bytes *atomic.Int64
}

func (w pathTrafficWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.bytes.Add(int64(n))
	return n, err
}

func waitSelectedPath(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	collector := metrics.NewCollector(t.TempDir(), "adaptive-test", "tcp", "client", nil, nil)
	deadline := time.Now().Add(timeout)
	var last *metrics.ManagedPathsStats
	for time.Now().Before(deadline) {
		last = collector.Snapshot().ManagedPaths
		if last != nil && last.Active == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("did not select %s: %+v", want, last)
}

func TestAdaptiveManagedPathsPreserveActiveForwardsAndRecoverNewConnectionsAfterFailure(t *testing.T) {
	cert, key := testCert(t)
	backend := startEchoBackend(t)
	internal, entry, firstPort, secondPort := freePort(t), freePort(t), freePort(t), freePort(t)
	s := baseServerConfig("tcp", internal, entry, backend.addr, "adaptive-path-test-token-0123456789abcdef")
	c := baseClientConfig("tcp", s.BindAddr, s.Token, nil)
	s.Heartbeat = 1
	c.Keepalive = 4
	s.Paths = []config.ManagedServerPath{
		{Name: "first", Short: config.ShortServerConfig{Listen: fmt.Sprintf("127.0.0.1:%d", firstPort), ServerName: "127.0.0.1", Certificate: cert, Key: key}},
		{Name: "second", Short: config.ShortServerConfig{Listen: fmt.Sprintf("127.0.0.1:%d", secondPort), ServerName: "127.0.0.1", Certificate: cert, Key: key}},
	}
	a, b := newControlledPath(t, s.Paths[0].Short.Listen), newControlledPath(t, s.Paths[1].Short.Listen)
	b.delay.Store(int64(250 * time.Millisecond))
	c.Paths = []config.ManagedClientPath{
		{Name: "first", Short: config.ShortClientConfig{Server: a.addr, CAFile: cert}},
		{Name: "second", Short: config.ShortClientConfig{Server: b.addr, CAFile: cert}},
	}
	c.PathPolicy = config.PathPolicy{ProbeInterval: 1, ProbeTimeout: 5, ProbeBytes: 1024, Failures: 2, Recoveries: 2, MinDwell: 1, Improvement: 10}
	tun := runPair(t, s, c, entry, internal)
	if err := tun.waitReady(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	waitSelectedPath(t, "first", 10*time.Second)
	active, err := net.DialTimeout("tcp", tun.Entry, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	check := func(label string) {
		t.Helper()
		active.SetDeadline(time.Now().Add(10 * time.Second))
		payload := []byte(label)
		if _, err := active.Write(payload); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(active, got); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("active forward %q lost: %v", label, err)
		}
	}
	check("before selection change")
	a.delay.Store(int64(250 * time.Millisecond))
	b.delay.Store(0)
	waitSelectedPath(t, "second", 25*time.Second)
	check("same active stream after healthy switch")
	if err := tun.roundTrip([]byte("new forward on second path")); err != nil {
		t.Fatal(err)
	}
	a.delay.Store(0)
	b.blocked.Store(true)
	waitSelectedPath(t, "first", 25*time.Second)
	// With a failed carrier, its outstanding pool and control operations can
	// still be unwinding after the selector withdraws it. Require bounded
	// verified recovery, without claiming zero interruption at that boundary.
	if err := tun.waitReady(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := tun.roundTrip([]byte("new forward after failure recovery")); err != nil {
		t.Fatal(err)
	}
	check("original first-path stream remains alive")
	active.Close()
	tun.Stop()
	waitPortFree(t, firstPort, 3*time.Second)
	waitPortFree(t, secondPort, 3*time.Second)
}
