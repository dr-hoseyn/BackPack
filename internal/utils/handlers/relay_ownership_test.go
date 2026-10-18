package handlers

import (
	"bytes"
	"io"
	"net"
	"testing"
)

// queuedWriteConn models a mux session whose caller is released by a timeout
// or connection close before the session consumes the queued frame.
type queuedWriteConn struct {
	net.Conn
	queued []byte
	err    error
	wrote  int
}

func (c *queuedWriteConn) Write(p []byte) (int, error) {
	c.queued = p
	return c.wrote, c.err
}

func (c *queuedWriteConn) Close() error { return nil }

func TestFailedRelayWriteKeepsQueuedPayloadImmutable(t *testing.T) {
	const payload = "frame still queued after stream termination"
	for _, tc := range []struct {
		name  string
		wrote int
		err   error
	}{
		{"closed", 0, net.ErrClosed},
		{"timeout-after-partial-write", 3, &net.OpError{Op: "write", Net: "smux", Err: timeoutError{}}},
		{"zero-write", 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := &halfConn{data: []byte(payload)}
			to := &queuedWriteConn{err: tc.err, wrote: tc.wrote}
			buf := make([]byte, relayBufferSize)
			reusable := transferBuffered(from, to, buf, quietLogger(), nil, 0, false)
			if reusable {
				// A subsequent relay reads into a returned buffer before the mux
				// send loop eventually copies its queued frame.
				copy(buf, bytes.Repeat([]byte{'x'}, len(payload)))
			}
			if string(to.queued) != payload {
				t.Fatalf("queued frame changed after failed write: %q", to.queued)
			}
			if reusable {
				t.Fatal("buffer with an outstanding frame was returned for reuse")
			}
		})
	}
}

func TestCompletedRelayWriteAllowsBufferReuse(t *testing.T) {
	from := &halfConn{data: []byte("complete frame"), err: io.EOF}
	to := &completedWriteConn{}
	if !transferBuffered(from, to, make([]byte, relayBufferSize), quietLogger(), nil, 0, false) {
		t.Fatal("completed writes must retain normal buffer reuse")
	}
	if string(to.received) != "complete frame" {
		t.Fatalf("received %q", to.received)
	}
}

type completedWriteConn struct {
	net.Conn
	received []byte
}

func (c *completedWriteConn) Write(p []byte) (int, error) {
	c.received = append(c.received, p...)
	return len(p), nil
}

func (c *completedWriteConn) Close() error { return nil }

type timeoutError struct{}

func (timeoutError) Error() string   { return "write timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
