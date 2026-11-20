package transport

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"
)

type muxTraceTestConn struct {
	net.Conn
	dst, src *net.TCPAddr
	sent     bytes.Buffer
}

func (c *muxTraceTestConn) LocalAddr() net.Addr         { return c.dst }
func (c *muxTraceTestConn) RemoteAddr() net.Addr        { return c.src }
func (c *muxTraceTestConn) Write(p []byte) (int, error) { return c.sent.Write(p) }

func TestWSSMuxAdmissionTraceReportsOnlyFirstLoopbackReply(t *testing.T) {
	conn := &muxTraceTestConn{
		dst: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 18081},
		src: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 24000},
	}
	accepted := time.Now().Add(-time.Second)
	local := LocalTCPConn{conn: conn, timeCreated: accepted.UnixMilli()}
	selected := accepted.Add(2 * time.Millisecond)
	if trace := startMuxAdmissionTrace(local, 0, selected); trace != nil {
		t.Fatal("disabled tracing created a trace")
	}
	if trace := startMuxAdmissionTrace(local, 18080, selected); trace != nil {
		t.Fatal("tracing leaked into another listener")
	}
	conn.src.IP = net.IPv4(192, 0, 2, 2)
	if trace := startMuxAdmissionTrace(local, 18081, selected); trace != nil {
		t.Fatal("tracing included a non-loopback sender")
	}
	conn.src.IP = net.IPv4(127, 0, 0, 1)
	trace := startMuxAdmissionTrace(local, 18081, selected)
	if trace == nil {
		t.Fatal("selected loopback connection was not traced")
	}
	trace.opened = selected.Add(3 * time.Millisecond)
	var report bytes.Buffer
	reply := &muxAdmissionReply{Conn: conn, trace: *trace, announced: trace.opened.Add(time.Millisecond), output: &report}
	for _, payload := range []string{"", "first", "second"} {
		if n, err := reply.Write([]byte(payload)); err != nil || n != len(payload) {
			t.Fatalf("write %q: %d, %v", payload, n, err)
		}
	}
	if reply.UnderlyingConn() != conn || conn.sent.String() != "firstsecond" {
		t.Fatal("the trace altered the forwarded bytes or socket identity")
	}
	if got := report.String(); strings.Count(got, "\n") != 1 ||
		!strings.HasPrefix(got, "WSSMUX_ADMISSION source_port=24000 queue_us=2000 open_us=3000 announce_us=1000 reply_us=") {
		t.Fatalf("unexpected report: %q", got)
	}
}
