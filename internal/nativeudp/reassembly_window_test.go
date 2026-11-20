package nativeudp

import (
	"bytes"
	"testing"
	"time"
)

func TestAuthenticatedNativeUDPDeliversThroughAFullReassemblyWindow(t *testing.T) {
	receiver, sender, wire, peer := linkPair(t)
	wire.mu.Lock()
	wire.hold = true
	wire.mu.Unlock()

	incomplete := bytes.Repeat([]byte{0x51}, fragmentSize+200)
	complete := bytes.Repeat([]byte{0x83}, len(incomplete))
	for range assemblyLimit {
		if err := sender.Write(incomplete); err != nil {
			t.Fatal(err)
		}
	}
	if err := sender.Write(complete); err != nil {
		t.Fatal(err)
	}
	wire.mu.Lock()
	packets := wire.packets
	wire.packets = nil
	wire.mu.Unlock()
	if len(packets) != (assemblyLimit+1)*2 {
		t.Fatalf("fixture produced %d encrypted fragments", len(packets))
	}
	deliver := func(index int) {
		t.Helper()
		if _, err := wire.PacketConn.WriteTo(packets[index], peer); err != nil {
			t.Fatal(err)
		}
	}
	waitForPartial := func(want int) {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			receiver.mu.Lock()
			got := len(receiver.assemblies)
			receiver.mu.Unlock()
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("receiver retained %d of %d expected partial datagrams", got, want)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// Drop the second fragment of each message and fill the table in
	// small groups so the test does not overflow the underlying socket.
	const group = 8
	for offset := 0; offset < assemblyLimit; offset += group {
		end := min(offset+group, assemblyLimit)
		for i := offset; i < end; i++ {
			deliver(i * 2)
		}
		waitForPartial(end)
	}

	// All fragments of the new datagram arrive, in reverse order.
	deliver(assemblyLimit*2 + 1)
	deliver(assemblyLimit * 2)
	result := make(chan []byte, 1)
	go func() {
		buf := make([]byte, len(complete))
		n, err := receiver.Read(buf)
		if err == nil {
			result <- buf[:n]
		}
	}()
	select {
	case got := <-result:
		if !bytes.Equal(got, complete) {
			t.Fatal("later datagram corrupted across full reassembly window")
		}
	case <-time.After(time.Second):
		t.Fatal("later complete datagram blocked by earlier fragment loss")
	}
}
