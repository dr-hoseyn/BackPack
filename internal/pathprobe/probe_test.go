package pathprobe

import (
	"net"
	"testing"
	"time"
)

func TestPathProbeVerifiesBothPayloadDirectionsWithoutClaimingAControlChannel(t *testing.T) {
	request, nonce, err := Request("private-probe-token", 16384)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	a.SetDeadline(time.Now().Add(time.Second))
	done := make(chan error, 1)
	go func() { done <- Serve(b, "private-probe-token", request) }()
	r, err := Check(a, "private-probe-token", nonce, time.Now())
	if err != nil || r.RTT <= 0 || r.BytesPerSecond <= 0 {
		t.Fatalf("verified transfer: %+v %v", r, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPathProbeRejectsWrongCredentialsAndOversizedPayloadBeforeAllocation(t *testing.T) {
	request, nonce, err := Request("wrong-probe-token", 1024)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() { done <- Serve(b, "private-probe-token", request) }()
	if _, err := Check(a, "wrong-probe-token", nonce, time.Now()); err == nil {
		t.Fatal("unauthenticated path marked healthy")
	}
	if err := <-done; err == nil {
		t.Fatal("wrong request credential accepted")
	}
	for _, n := range []int{0, 1023, 65537} {
		if _, _, err := Request("private-probe-token", n); err == nil {
			t.Fatal("out-of-bounds payload accepted")
		}
	}
}
