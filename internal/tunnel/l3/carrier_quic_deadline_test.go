package l3

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func quicDeadlinePair(t testing.TB) (*quicCarrier, *quicCarrier, net.Addr, net.Addr) {
	t.Helper()
	listener, _, err := openQuic(Config{Mode: ModeListen, Addr: "127.0.0.1:0", Carrier: CarrierQuic})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	dialer, target, err := openQuic(Config{Mode: ModeDial, Addr: listener.LocalAddr().String(), Carrier: CarrierQuic})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dialer.Close() })
	if _, err := dialer.WriteTo([]byte("warm"), target); err != nil {
		t.Fatal(err)
	}
	listener.SetReadDeadline(time.Now().Add(2 * time.Second))
	var buf [32]byte
	_, remote, err := listener.ReadFrom(buf[:])
	if err != nil {
		t.Fatal(err)
	}
	listener.SetReadDeadline(time.Time{})
	return listener.(*quicCarrier), dialer.(*quicCarrier), remote, target
}

func TestQuicReadDeadlineInterruptsPendingReads(t *testing.T) {
	listener, dialer, _, _ := quicDeadlinePair(t)
	for name, carrier := range map[string]*quicCarrier{"listen": listener, "dial": dialer} {
		t.Run(name, func(t *testing.T) {
			const readers = 4
			done := make(chan error, readers)
			for i := 0; i < readers; i++ {
				go func() { _, _, err := carrier.ReadFrom(make([]byte, 32)); done <- err }()
			}
			// Verify the idle read is pending before changing its deadline.
			select {
			case err := <-done:
				t.Fatalf("idle read returned early: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			if err := carrier.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < readers; i++ {
				select {
				case err := <-done:
					if !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatalf("read error = %v, want deadline exceeded", err)
					}
				case <-time.After(500 * time.Millisecond):
					t.Fatal("deadline change did not interrupt every pending ReadFrom")
				}
			}
		})
	}
}

func TestQuicReadDeadlineCanBeClearedOrExtendedWhileReading(t *testing.T) {
	for _, clear := range []bool{true, false} {
		listener, dialer, remote, target := quicDeadlinePair(t)
		for _, tc := range []struct {
			name           string
			reader, writer *quicCarrier
			target         net.Addr
		}{
			{"listen", listener, dialer, target}, {"dial", dialer, listener, remote},
		} {
			t.Run(tc.name+map[bool]string{true: "/clear", false: "/extend"}[clear], func(t *testing.T) {
				tc.reader.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				done := make(chan error, 1)
				go func() {
					var packet [4]byte
					_, _, err := tc.reader.ReadFrom(packet[:])
					if err == nil && string(packet[:]) != "live" {
						err = errors.New("packet changed")
					}
					done <- err
				}()
				time.Sleep(20 * time.Millisecond)
				deadline := time.Now().Add(2 * time.Second)
				if clear {
					deadline = time.Time{}
				}
				tc.reader.SetReadDeadline(deadline)
				select {
				case err := <-done:
					t.Fatalf("old deadline interrupted updated read: %v", err)
				case <-time.After(400 * time.Millisecond):
				}
				if _, err := tc.writer.WriteTo([]byte("live"), tc.target); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("read did not recover after deadline update")
				}
			})
		}
	}
}

func BenchmarkQuicCarrierReadWithDeadline(b *testing.B) {
	listener, _, _, _ := quicDeadlinePair(b)
	listener.SetReadDeadline(time.Now().Add(time.Hour))
	packet := make([]byte, 1024)
	buffer := make([]byte, len(packet))
	from := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
	b.SetBytes(int64(len(packet)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		listener.in <- quicDatagram{data: packet, from: from}
		if _, _, err := listener.ReadFrom(buffer); err != nil {
			b.Fatal(err)
		}
	}
}

func TestQuicReadDeadlineAfterClose(t *testing.T) {
	listener, dialer, _, _ := quicDeadlinePair(t)
	for _, carrier := range []*quicCarrier{listener, dialer} {
		carrier.Close()
		if err := carrier.SetReadDeadline(time.Now().Add(time.Second)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("deadline after close = %v, want net.ErrClosed", err)
		}
		if _, _, err := carrier.ReadFrom(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read after close = %v, want net.ErrClosed", err)
		}
	}
}
