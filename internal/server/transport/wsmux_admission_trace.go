package transport

import (
	"fmt"
	"io"
	"net"
	"time"

	"github.com/backpack/backpack/internal/utils/network"
)

// The trace is opt-in for one loopback forwarded port. It reports completed
// admissions and their first response, without inspecting application bytes.
type muxAdmissionTrace struct {
	sourcePort  int
	queueUS     int64
	selected    time.Time
	opened      time.Time
	carrierPort int
	initial     network.MuxTCPSnapshot
}

func startMuxAdmissionTrace(local LocalTCPConn, port int, selected time.Time) *muxAdmissionTrace {
	if port == 0 || local.conn == nil {
		return nil
	}
	addr, ok := local.conn.LocalAddr().(*net.TCPAddr)
	if !ok || addr.Port != port || !addr.IP.IsLoopback() {
		return nil
	}
	source, ok := local.conn.RemoteAddr().(*net.TCPAddr)
	if !ok || !source.IP.IsLoopback() {
		return nil
	}
	return &muxAdmissionTrace{
		sourcePort: source.Port,
		queueUS:    max(0, selected.UnixMilli()-local.timeCreated) * 1000,
		selected:   selected,
	}
}

func (t *muxAdmissionTrace) sampleCarrier(conn net.Conn) {
	if conn == nil {
		return
	}
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		t.carrierPort = addr.Port
	}
	t.initial = network.SampleMuxTCP(conn)
}

type muxAdmissionReply struct {
	net.Conn
	trace     muxAdmissionTrace
	announced time.Time
	output    io.Writer
	carrier   net.Conn
	streamID  uint32
	reported  bool // only the relay's writer accesses this
}

func (c *muxAdmissionReply) UnderlyingConn() net.Conn { return c.Conn }

func (c *muxAdmissionReply) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && !c.reported {
		first := time.Now()
		c.reported = true
		end := network.SampleMuxTCP(c.carrier)
		fmt.Fprintf(c.output, "WSSMUX_ADMISSION source_port=%d queue_us=%d open_us=%d announce_us=%d reply_us=%d carrier_port=%d stream_id=%d tcp_available=%t notsent_start=%d notsent_end=%d retrans_start=%d retrans_end=%d tcp_rtt_start_us=%d tcp_rtt_end_us=%d\n",
			c.trace.sourcePort, c.trace.queueUS, c.trace.opened.Sub(c.trace.selected).Microseconds(),
			c.announced.Sub(c.trace.opened).Microseconds(), first.Sub(c.announced).Microseconds(),
			c.trace.carrierPort, c.streamID, c.trace.initial.Available && end.Available,
			c.trace.initial.NotSentBytes, end.NotSentBytes, c.trace.initial.TotalRetrans, end.TotalRetrans,
			c.trace.initial.RTTUS, end.RTTUS)
	}
	return n, err
}
