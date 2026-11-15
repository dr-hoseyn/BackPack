package l3

import (
	"net"
	"testing"
	"time"
)

// UDP methods exist on every platform, even when no recvmmsg/sendmmsg wrapper
// was installed. Method presence alone is not a usable batching capability.
func TestUDPWithoutBatchWrapperHasNoBatchCapability(t *testing.T) {
	carrier := &udpCarrier{UDPConn: localUDP(t)}
	if asBatchReader(carrier) != nil {
		t.Error("an unbatched socket advertised batch reads")
	}
	if asBatchWriter(carrier) != nil {
		t.Error("an unbatched socket advertised batch writes")
	}
}

func unbatchedMultipath(t *testing.T) (DatagramCarrier, *net.UDPConn, *net.UDPConn) {
	t.Helper()
	a, b := localUDP(t), localUDP(t)
	first, second := localUDP(t), localUDP(t)
	carrier := newMultipathCarrier([]DatagramCarrier{
		&pinnedCarrier{DatagramCarrier: &udpCarrier{UDPConn: a}, peer: first.LocalAddr()},
		&pinnedCarrier{DatagramCarrier: &udpCarrier{UDPConn: b}, peer: second.LocalAddr()},
	}, first.LocalAddr())
	t.Cleanup(func() { carrier.Close() })
	return carrier, first, a
}

func TestMultipathReadsWithoutBatchSocket(t *testing.T) {
	carrier, peer, socket := unbatchedMultipath(t)
	if _, err := peer.WriteToUDP([]byte("live"), socket.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	carrier.SetReadDeadline(time.Now().Add(time.Second))
	var packet [32]byte
	n, _, err := carrier.ReadFrom(packet[:])
	if err != nil || string(packet[:n]) != "live" {
		t.Fatalf("multipath without batching lost traffic: %q, %v", packet[:n], err)
	}
}

func TestMultipathWritesWithoutBatchSocket(t *testing.T) {
	carrier, peer, _ := unbatchedMultipath(t)
	packets := [][]byte{[]byte("first"), []byte("second"), []byte("third")}
	writer := asBatchWriter(carrier)
	if writer == nil {
		t.Fatal("multipath must keep its own batch entry point")
	}
	if n, err := writer.WriteBatch(packets, nil); err != nil || n != len(packets) {
		t.Fatalf("single-packet fallback sent %d packets: %v", n, err)
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	for _, want := range packets {
		var packet [32]byte
		n, _, err := peer.ReadFrom(packet[:])
		if err != nil || string(packet[:n]) != string(want) {
			t.Fatalf("fallback changed packet order/content: %q, want %q, err=%v", packet[:n], want, err)
		}
	}
}
