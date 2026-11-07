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
