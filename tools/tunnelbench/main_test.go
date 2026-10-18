package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func echoFixture(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("echo server did not close active connections")
		}
	})
	return l.Addr().String()
}
func TestConcurrentEchoBenchmarkVerifiesBytesAndSamplesLocalResources(t *testing.T) {
	address := echoFixture(t)
	r, err := runCase(t.Context(), address, 4, 200*time.Millisecond, 4096, []int32{int32(os.Getpid())})
	if err != nil || r.Failures != 0 || r.VerifiedBytes == 0 || r.Mbps <= 0 || r.Parallel != 4 {
		t.Fatalf("verified benchmark: %+v %v", r, err)
	}
	if r.Resources == nil || r.Resources.Samples < 2 || r.Resources.PeakRSS == 0 {
		t.Fatalf("missing process measurements: %+v", r.Resources)
	}
}
func TestCorruptedEchoNeverBecomesVerifiedThroughput(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		body := make([]byte, 1024)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		body[0] ^= 255
		_, _ = conn.Write(body)
	}()
	r, err := runCase(t.Context(), l.Addr().String(), 1, time.Second, 1024, nil)
	if err != nil || r.Failures != 1 || r.VerifiedBytes != 0 || r.Mbps != 0 {
		t.Fatalf("corruption claimed as throughput: %+v %v", r, err)
	}
	<-done
}
func TestBenchmarkCancellationStopsBlockedReaders(t *testing.T) {
	address := echoFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := runCase(ctx, address, 2, 10*time.Second, 1024, nil); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("workers did not stop")
	}
}
func TestBenchmarkRejectsUnboundedOrAmbiguousWorkloads(t *testing.T) {
	for _, args := range [][]string{{}, {"-target", "a=127.0.0.1:1", "-parallel", "257"}, {"-target", "a=127.0.0.1:1", "-duration", "0s"}, {"-target", "a=127.0.0.1:1", "-target", "a=127.0.0.1:2"}, {"-target", "a=127.0.0.1:1", "-pid", "b=123"}} {
		if err := run(t.Context(), args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid flags accepted: %s", strings.Join(args, " "))
		}
	}
}
