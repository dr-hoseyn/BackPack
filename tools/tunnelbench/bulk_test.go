package main

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net"
	"os"
	"testing"
	"time"
)

func TestBulkDirectionsVerifyReceiverBytesAndSampleResources(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- serveMode(ctx, l, true) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for _, direction := range []byte{'U', 'D'} {
		r, err := runBulkCase(t.Context(), l.Addr().String(), 4, 300*time.Millisecond, 100*time.Millisecond, 2*time.Second, direction, []int32{int32(os.Getpid())})
		if err != nil || r.Failures != 0 || r.VerifiedBytes == 0 || r.BulkMbps <= 0 || r.Mbps != 0 {
			t.Fatalf("%c: %+v %v", direction, r, err)
		}
		if r.Resources == nil || r.Resources.Samples < 2 || r.Resources.PeakRSS == 0 {
			t.Fatalf("missing measurements: %+v", r.Resources)
		}
		if r.Seconds > 1 || r.Seconds < 0.25 {
			t.Fatal("warmup included in measurement", r.Seconds)
		}
	}
}

func TestBulkRejectsCorruptDownloadAndInvalidUploadProof(t *testing.T) {
	for _, direction := range []byte{'D', 'U'} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(2 * time.Second))
			head := make([]byte, len(bulkMagic)+1+bulkBlock)
			if _, err = io.ReadFull(c, head); err != nil {
				return
			}
			io.WriteString(c, bulkAck)
			var start [1]byte
			if _, err = io.ReadFull(c, start[:]); err != nil {
				return
			}
			if direction == 'D' {
				head[len(head)-1] ^= 255
				c.Write(head[len(bulkMagic)+1:])
			} else {
				var ack [8]byte
				binary.BigEndian.PutUint64(ack[:], math.MaxUint64)
				c.Write(ack[:])
			}
		}()
		r, err := runBulkCase(t.Context(), l.Addr().String(), 1, time.Second, 0, time.Second, direction, nil)
		l.Close()
		<-done
		if err == nil || r.Failures == 0 {
			t.Fatalf("%c corruption accepted: %+v %v", direction, r, err)
		}
	}
}

func TestBulkCancellationInterruptsHandshake(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = runBulkCase(ctx, l.Addr().String(), 1, time.Second, 0, time.Second, 'D', nil)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("cancellation not bounded: %v", err)
	}
	<-done
}

func TestBulkUploadWriterStallFailsEvenWhenReceiverKeepsSendingZeroACKs(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		head := make([]byte, len(bulkMagic)+1+bulkBlock)
		if _, err = io.ReadFull(c, head); err != nil {
			return
		}
		if _, err = io.WriteString(c, bulkAck); err != nil {
			return
		}
		var start [1]byte
		if _, err = io.ReadFull(c, start[:]); err != nil {
			return
		}
		// Never consume the upload. An active ACK reader must not mask the
		// sender's write deadline and turn this into a successful partial run.
		var ack [8]byte
		for {
			if _, err = c.Write(ack[:]); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	_, err = runBulkCase(t.Context(), l.Addr().String(), 1, 3*time.Second, 0, 80*time.Millisecond, 'U', nil)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("writer stall was hidden: %v", err)
	}
	<-done
}
