package network

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

type wsDeadlineProbe struct {
	net.Conn
	deadline time.Time
}

func (c *wsDeadlineProbe) SetWriteDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

func (*wsDeadlineProbe) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestWSMuxKeepsTheTighterFrameAndStreamWriteDeadlines(t *testing.T) {
	raw := &wsDeadlineProbe{}
	conn := &streamDeadlineConn{Conn: raw}
	now := time.Now()
	frame := now.Add(time.Second)
	stream := now.Add(40 * time.Second)
	if err := conn.SetWriteDeadline(frame); err != nil {
		t.Fatal(err)
	}
	if err := conn.setStreamWriteDeadline(stream); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(frame) {
		t.Fatalf("refreshing the mux deadline extended the in-flight frame deadline to %v", raw.deadline)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(stream) {
		t.Fatalf("clearing the frame deadline removed the mux limit: %v", raw.deadline)
	}
	if err := conn.SetWriteDeadline(frame); err != nil {
		t.Fatal(err)
	}
	if err := conn.setStreamWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !raw.deadline.Equal(frame) {
		t.Fatalf("clearing the mux deadline removed the frame limit: %v", raw.deadline)
	}
}

func TestWSMuxReportsIncompleteWireWrites(t *testing.T) {
	conn := &streamDeadlineConn{Conn: &wsDeadlineProbe{}}
	n, err := conn.Write([]byte("frame"))
	if n != 4 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("incomplete wire write reported %d, %v", n, err)
	}
}

type recordingDeadlineConn struct {
	net.Conn
	writes []time.Time
	err    error
}

func (c *recordingDeadlineConn) SetWriteDeadline(t time.Time) error {
	c.writes = append(c.writes, t)
	return c.err
}
func (c *recordingDeadlineConn) SetDeadline(t time.Time) error { return c.SetWriteDeadline(t) }
func (*recordingDeadlineConn) Close() error                    { return nil }

func TestWSMuxFrameRefreshesDoNotReapplyUnchangedSocketDeadlines(t *testing.T) {
	raw := &recordingDeadlineConn{}
	c := &streamDeadlineConn{Conn: raw}
	deadline := time.Now().Add(time.Second)
	if err := c.setStreamWriteDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if err := c.SetWriteDeadline(time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(raw.writes) != 1 || !raw.writes[0].Equal(deadline) {
		t.Fatal("unchanged frame deadlines repeatedly mutated the socket timer")
	}
	// A temporary control-frame deadline may shorten the stream's deadline.
	earlier := deadline.Add(-time.Millisecond)
	_ = c.SetWriteDeadline(earlier)
	_ = c.SetWriteDeadline(time.Time{})
	if len(raw.writes) != 3 || !raw.writes[1].Equal(earlier) || !raw.writes[2].Equal(deadline) {
		t.Fatal("control-frame deadline was not restored to the stream budget")
	}
	// A combined setup deadline must invalidate the old applied value.
	_ = c.SetDeadline(deadline.Add(time.Second))
	_ = c.SetWriteDeadline(time.Time{})
	if len(raw.writes) != 5 || !raw.writes[4].Equal(deadline) {
		t.Fatal("combined deadline left a stale cached frame deadline")
	}
	_ = c.setStreamWriteDeadline(time.Time{})
	_ = c.SetWriteDeadline(time.Time{})
	if len(raw.writes) != 6 || !raw.writes[5].IsZero() {
		t.Fatal("clearing the stream deadline did not reach the socket")
	}
	c.Close()
	if err := c.SetWriteDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed stream returned cached success: %v", err)
	}
}

func TestWSMuxFailedDeadlineChangesAreRetried(t *testing.T) {
	raw := &recordingDeadlineConn{err: net.ErrClosed}
	c := &streamDeadlineConn{Conn: raw}
	for range 2 {
		if err := c.SetWriteDeadline(time.Time{}); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("deadline error hidden: %v", err)
		}
	}
	if len(raw.writes) != 2 {
		t.Fatal("a failed deadline change was cached")
	}
}

func BenchmarkWSMuxFrameDeadlineRefresh(b *testing.B) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer listener.Close()
	peer, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer peer.Close()
	raw, err := listener.Accept()
	if err != nil {
		b.Fatal(err)
	}
	defer raw.Close()
	c := &streamDeadlineConn{Conn: raw}
	_ = c.SetWriteDeadline(time.Time{})
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := c.SetWriteDeadline(time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}
