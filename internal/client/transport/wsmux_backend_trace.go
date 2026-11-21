package transport

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/backpack/backpack/internal/utils/network"
	"github.com/xtaci/smux"
)

// The client-side trace is restricted to one explicitly selected loopback
// backend. The stream and carrier IDs come from the existing connections.
type wsmuxBackendTrace struct {
	carrier     net.Conn
	carrierPort int
	streamID    uint32
	initial     network.MuxTCPSnapshot
	started     time.Time
}

func startWSMuxBackendTrace(user net.Conn, addr string, port int, o backendOpts) *wsmuxBackendTrace {
	if o.tracePort == 0 || port != o.tracePort || o.traceCarrier == nil || !isLoopback(addr) {
		return nil
	}
	stream, ok := user.(*smux.Stream)
	if !ok {
		return nil
	}
	carrier, ok := o.traceCarrier.LocalAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return &wsmuxBackendTrace{
		carrier: o.traceCarrier, carrierPort: carrier.Port, streamID: stream.ID(),
		initial: network.SampleMuxTCP(o.traceCarrier), started: time.Now(),
	}
}

func (t *wsmuxBackendTrace) wrap(conn net.Conn) net.Conn {
	connected := time.Now()
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		return conn
	}
	return &wsmuxBackendReply{
		Conn: conn, trace: *t, connected: connected, output: os.Stderr,
	}
}

type wsmuxBackendReply struct {
	net.Conn
	trace     wsmuxBackendTrace
	connected time.Time
	output    io.Writer
	reported  bool // only the relay's reader accesses this
}

func (c *wsmuxBackendReply) UnderlyingConn() net.Conn { return c.Conn }

func (c *wsmuxBackendReply) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && !c.reported {
		first := time.Now()
		c.reported = true
		end := network.SampleMuxTCP(c.trace.carrier)
		fmt.Fprintf(c.output, "WSSMUX_BACKEND carrier_port=%d stream_id=%d dial_us=%d backend_reply_us=%d tcp_available=%t notsent_start=%d notsent_end=%d retrans_start=%d retrans_end=%d tcp_rtt_start_us=%d tcp_rtt_end_us=%d\n",
			c.trace.carrierPort, c.trace.streamID, c.connected.Sub(c.trace.started).Microseconds(),
			first.Sub(c.connected).Microseconds(), c.trace.initial.Available && end.Available,
			c.trace.initial.NotSentBytes, end.NotSentBytes, c.trace.initial.TotalRetrans, end.TotalRetrans,
			c.trace.initial.RTTUS, end.RTTUS)
	}
	return n, err
}
