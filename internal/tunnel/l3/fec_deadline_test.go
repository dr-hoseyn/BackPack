package l3

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// Two losses create two reconstructed packets at once. Reading the first
// leaves the other inside FEC, so its state must obey the PacketConn contract
// independently of the underlying socket's next read.
func fecWithPendingRecovery(t *testing.T) *fecCarrier {
	t.Helper()
	send, recv := newLossyPair(nil)
	send.drop = func(n int) bool { return n == 0 || n == 1 }
	cfg := FECConfig{Data: 4, Parity: 2}
	tx, err := newFECCarrier(send, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rx, err := newFECCarrier(recv, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Close(); rx.Close() })
	for _, payload := range []string{"lost-zero", "lost-one", "two", "three"} {
		if _, err := tx.WriteTo([]byte(payload), recv.addr); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 64)
	for _, want := range []string{"two", "three", "lost-zero"} {
		n, _, err := rx.ReadFrom(buf)
		if err != nil || string(buf[:n]) != want {
			t.Fatalf("recovery setup: got %q, err %v; want %q", buf[:n], err, want)
		}
	}
	return rx.(*fecCarrier)
}

func TestFECBufferedRecoveryHonorsReadDeadline(t *testing.T) {
	for _, both := range []bool{false, true} {
		name := "read"
		if both {
			name = "both"
		}
		t.Run(name, func(t *testing.T) {
			rx := fecWithPendingRecovery(t)
			set := rx.SetReadDeadline
			if both {
				set = rx.SetDeadline
			}
			if err := set(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 64)
			if _, _, err := rx.ReadFrom(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("buffered packet bypassed expired deadline: %v", err)
			}
			if err := set(time.Time{}); err != nil {
				t.Fatal(err)
			}
			n, _, err := rx.ReadFrom(buf)
			if err != nil || string(buf[:n]) != "lost-one" {
				t.Fatalf("deadline consumed the pending recovery: got %q, err %v", buf[:n], err)
			}
		})
	}
}

func TestFECBufferedRecoveryStopsAfterClose(t *testing.T) {
	rx := fecWithPendingRecovery(t)
	if err := rx.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rx.ReadFrom(make([]byte, 64)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed FEC carrier returned a buffered packet: %v", err)
	}
}

type fecObservedRead struct {
	DatagramCarrier
	started chan struct{}
}

func (c *fecObservedRead) ReadFrom(p []byte) (int, net.Addr, error) {
	close(c.started)
	return c.DatagramCarrier.ReadFrom(p)
}

func TestFECCloseInterruptsSocketRead(t *testing.T) {
	below, err := listenUDP("127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	observed := &fecObservedRead{DatagramCarrier: below, started: make(chan struct{})}
	rx, err := newFECCarrier(observed, FECConfig{Data: 4, Parity: 2}, nil)
	if err != nil {
		below.Close()
		t.Fatal(err)
	}
	defer rx.Close()
	done := make(chan error, 1)
	go func() { _, _, err := rx.ReadFrom(make([]byte, 64)); done <- err }()
	select {
	case <-observed.started:
	case <-time.After(time.Second):
		t.Fatal("read did not reach the socket")
	}
	if err := rx.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("closed socket read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("closing FEC retained its blocked socket read")
	}
}
