package network

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// Linux rejects a one-byte TCP MSS. That rejection must not be overwritten by
// RawConn.Control returning nil on either side of the tunnel.
func TestTCPRejectsInvalidSocketTuning(t *testing.T) {
	listener, err := ListenWithBuffers("tcp", "127.0.0.1:0", 0, 0, 1, time.Second, false)
	if listener != nil {
		listener.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "MSS") {
		t.Fatalf("invalid listener MSS was not reported: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := TcpDialer(context.Background(), ln.Addr().String(), time.Second, time.Second, true, 1, 0, 0, 1)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "MSS") {
		t.Fatalf("invalid dial MSS was not reported: %v", err)
	}
}
