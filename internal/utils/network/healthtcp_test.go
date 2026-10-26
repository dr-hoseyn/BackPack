package network

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestTCPHealthWorksWithoutPingAndChecksThePort(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux ping fixture")
	}
	dir := t.TempDir()
	ping := filepath.Join(dir, "ping")
	if err := os.WriteFile(ping, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	probe := TCPHealthProbe(nil, 100*time.Millisecond)
	if s := probe(t.Context(), l.Addr().String(), 3); !s.Reachable || s.LossPct != 0 {
		t.Fatalf("healthy TCP port with blocked ping: %+v", s)
	}
	l.Close()
	if err := os.WriteFile(ping, []byte("#!/bin/sh\necho '0% packet loss'\necho 'rtt min/avg/max/mdev = 1/1/1/0 ms'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if s := probe(t.Context(), l.Addr().String(), 3); s.Reachable {
		t.Fatalf("ping succeeded but the tunnel port is closed: %+v", s)
	}
}

func TestTCPHealthUsesTheProxyForUnresolvableTargets(t *testing.T) {
	proxy, target := fakeHTTPProxy(t, "200 Connection Established")
	p, err := ParseProxy("http://" + proxy)
	if err != nil {
		t.Fatal(err)
	}
	s := TCPHealthProbe(&Outbound{Proxy: p}, time.Second)(t.Context(), "tunnel.invalid:443", 1)
	if !s.Reachable {
		t.Fatalf("proxy route was measured as a direct connection: %+v", s)
	}
	select {
	case got := <-target:
		if got != "tunnel.invalid:443" {
			t.Fatalf("probed wrong destination %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy received no probe")
	}
}

func TestHealthProbesAreBoundedAndCancellationReleasesWorkers(t *testing.T) {
	e := NewEndpoints("a", "b", "c", "d", "e", "f")
	var active, peak atomic.Int64
	entered := make(chan struct{}, 6)
	e.SetHealthProbe(func(ctx context.Context, addr string, _ int) EndpointScore {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		entered <- struct{}{}
		<-ctx.Done()
		return EndpointScore{Addr: addr}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.measureHealth(ctx, e.healthProbe.Load()) }()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("one blocked address delayed probing the other routes")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health probe workers did not stop after cancellation")
	}
	if active.Load() != 0 || peak.Load() > 4 {
		t.Fatalf("unbounded or leaked probes: active=%d peak=%d", active.Load(), peak.Load())
	}
}
