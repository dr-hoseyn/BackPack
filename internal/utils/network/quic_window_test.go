package network

import "testing"

func TestQUICFlowControlLeavesRoomFor500MbitOnADistantPath(t *testing.T) {
	cfg := (QUICSettings{}).quicConfig()
	const required = 500_000_000 / 8 / 5
	if cfg.MaxStreamReceiveWindow < required || cfg.MaxConnectionReceiveWindow < 2*required {
		t.Fatal("QUIC flow control is below the 500 Mbit/s, 200 ms bandwidth-delay product")
	}
	if cfg.InitialStreamReceiveWindow >= cfg.MaxStreamReceiveWindow {
		t.Fatal("large receive windows must grow on demand")
	}
}
