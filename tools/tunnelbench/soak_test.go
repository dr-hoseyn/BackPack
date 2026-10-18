package main

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestSoakCancellationCannotClaimCompletedObservation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := runSoak(ctx, "127.0.0.1:1", time.Second, 100*time.Millisecond, 128, nil)
	if err == nil || r.Soak.Complete || r.Soak.Received != 0 {
		t.Fatal("cancelled run claimed completion")
	}
}
func TestSoakRecoversWithNewConnections(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serve(ctx, l)
	r, err := runSoak(context.Background(), l.Addr().String(), 2100*time.Millisecond, 200*time.Millisecond, 512, nil)
	if err != nil || !r.Soak.Complete || r.Soak.Received < 2 || r.Failures != 0 {
		t.Fatalf("soak result: %+v %v", r, err)
	}
}
