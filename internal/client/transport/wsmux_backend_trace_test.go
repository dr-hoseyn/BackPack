package transport

import (
	"bytes"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type backendTraceTestConn struct {
	net.Conn
	reader io.Reader
}

func (c *backendTraceTestConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func TestWSSMuxBackendTraceReportsOnlyFirstBackendBytes(t *testing.T) {
	conn := &backendTraceTestConn{reader: strings.NewReader("firstsecond")}
	now := time.Now()
	var report bytes.Buffer
	trace := &wsmuxBackendReply{
		Conn: conn, output: &report, connected: now,
		trace: wsmuxBackendTrace{carrierPort: 24000, streamID: 3, started: now.Add(-time.Millisecond)},
	}
	if n, err := trace.Read(nil); n != 0 || err != nil || report.Len() != 0 {
		t.Fatalf("empty read reported a reply: %d, %v, %q", n, err, report.String())
	}
	for _, want := range []string{"first", "second"} {
		buf := make([]byte, len(want))
		if _, err := io.ReadFull(trace, buf); err != nil || string(buf) != want {
			t.Fatalf("read %q: %q, %v", want, buf, err)
		}
	}
	if trace.UnderlyingConn() != conn {
		t.Fatal("the relay lost the backend socket identity")
	}
	if got := report.String(); strings.Count(got, "\n") != 1 ||
		!strings.HasPrefix(got, "WSSMUX_BACKEND carrier_port=24000 stream_id=3 dial_us=1000 backend_reply_us=") ||
		!strings.Contains(got, " tcp_available=false ") {
		t.Fatalf("unexpected report: %q", got)
	}
}
