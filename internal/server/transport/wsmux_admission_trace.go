package transport

import (
	"fmt"
	"io"
	"net"
	"time"
)

// The trace is opt-in for one loopback forwarded port. It reports completed
// admissions and their first response, without inspecting application bytes.
type muxAdmissionTrace struct {
	sourcePort int
	queueUS    int64
	selected   time.Time
	opened     time.Time
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

type muxAdmissionReply struct {
	net.Conn
	trace     muxAdmissionTrace
	announced time.Time
	output    io.Writer
	reported  bool // only the relay's writer accesses this
}

func (c *muxAdmissionReply) UnderlyingConn() net.Conn { return c.Conn }

func (c *muxAdmissionReply) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && !c.reported {
		first := time.Now()
		c.reported = true
		fmt.Fprintf(c.output, "WSSMUX_ADMISSION source_port=%d queue_us=%d open_us=%d announce_us=%d reply_us=%d\n",
			c.trace.sourcePort, c.trace.queueUS, c.trace.opened.Sub(c.trace.selected).Microseconds(),
			c.announced.Sub(c.trace.opened).Microseconds(), first.Sub(c.announced).Microseconds())
	}
	return n, err
}
