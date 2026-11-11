package network

import (
	"crypto/tls"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

func TestWSMuxPressureFindsTheSocketUnderTLSAndDeadlineWrappers(t *testing.T) {
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	secure := tls.Client(local, &tls.Config{ServerName: "example.test"})
	wrapped := &streamDeadlineConn{Conn: secure}
	if got := tcpMuxPressureConn(wrapped); got != local {
		t.Fatal("TLS/deadline wrappers hid the carrier's socket from congestion sampling")
	}
	if wrapped.Conn != secure || secure.NetConn() != local {
		t.Fatal("sampling changed the encrypted I/O path")
	}
	cycle := &streamDeadlineConn{}
	cycle.Conn = cycle
	if tcpMuxPressureConn(cycle) != nil {
		t.Fatal("a cyclic wrapper chain did not stop at the observation bound")
	}
}

func TestTCPMuxPressureConfirmsBacklogAndCachesKernelSamples(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reads, next, queued := 0, 64, true
		pressure := tcpMuxPressureSampler(func() (int, bool) {
			reads++
			return next, queued
		})
		// An app-limited heartbeat followed by a single burst must not
		// push the next user onto a cold spare (whose cost starts at 16).
		if got := pressure(); got >= 16 {
			t.Fatalf("unconfirmed backlog displaced warm capacity: %d", got)
		}
		for range 100 {
			pressure()
		}
		if reads != 1 {
			t.Fatalf("one burst sampled the kernel %d times", reads)
		}
		time.Sleep(200 * time.Millisecond)
		if got := pressure(); got != 64 {
			t.Fatalf("persistent congestion remained hidden: %d", got)
		}
		next, queued = 0, false
		time.Sleep(200 * time.Millisecond)
		if got := pressure(); got != 0 {
			t.Fatalf("drained queue retained stale pressure: %d", got)
		}
		next, queued = 64, true
		time.Sleep(200 * time.Millisecond)
		if got := pressure(); got >= 16 {
			t.Fatalf("a later short burst reused old congestion: %d", got)
		}
	})
}
