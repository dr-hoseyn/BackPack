package network

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

type failedNoiseWriter struct {
	net.Conn
	queued []byte
	err    error
	short  bool
	calls  int
	closed bool
}

func (c *failedNoiseWriter) Write(p []byte) (int, error) {
	c.calls++
	c.queued = p
	if c.short {
		return len(p) / 2, nil
	}
	return 0, c.err
}

func (c *failedNoiseWriter) Close() error {
	c.closed = true
	return c.Conn.Close()
}

func TestFailedNoiseRecordEndsTheConnectionBeforeAnotherWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		short bool
	}{
		{name: "timeout", err: os.ErrDeadlineExceeded},
		{name: "closed", err: net.ErrClosed},
		{name: "short-write", err: io.ErrShortWrite, short: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, sc, cerr, serr := noisePair(t, "record-failure-test", "record-failure-test")
			if cerr != nil || serr != nil {
				t.Fatalf("handshake failed: client=%v server=%v", cerr, serr)
			}
			defer cc.Close()
			defer sc.Close()
			client := cc.(*noiseConn)
			writer := &failedNoiseWriter{Conn: client.Conn, err: tc.err, short: tc.short}
			client.Conn = writer
			if n, err := client.Write([]byte("first record")); n != 0 || !errors.Is(err, tc.err) {
				t.Fatalf("failed record write = %d, %v; want 0, %v", n, err, tc.err)
			}
			queued := bytes.Clone(writer.queued)
			if n, err := client.Write([]byte("later record")); n != 0 || !errors.Is(err, tc.err) {
				t.Fatalf("write after record failure = %d, %v; want original failure", n, err)
			}
			if writer.calls != 1 || !bytes.Equal(writer.queued, queued) {
				t.Error("a later write reused ciphertext still owned by the failed write")
			}
			if !writer.closed {
				t.Error("failed record left the underlying connection open")
			}
			// A peer must see the failed carrier end so the transport can
			// reconnect, instead of waiting on an irrecoverable record forever.
			_ = sc.SetReadDeadline(time.Now().Add(time.Second))
			_, err := sc.Read(make([]byte, 1))
			if err == nil {
				t.Fatal("peer did not see the failed carrier end")
			}
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatal("peer waited until its deadline after the record write failed")
			}
		})
	}
}

func TestNoiseWriteTimeoutCannotResumeWithTheWrongRecordNonce(t *testing.T) {
	cc, sc, cerr, serr := noisePair(t, "record-timeout-test", "record-timeout-test")
	if cerr != nil || serr != nil {
		t.Fatalf("handshake failed: client=%v server=%v", cerr, serr)
	}
	defer cc.Close()
	defer sc.Close()
	if err := cc.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, firstErr := cc.Write([]byte("record that never reaches the peer"))
	var timeout net.Error
	if !errors.As(firstErr, &timeout) || !timeout.Timeout() {
		t.Fatalf("expired write deadline returned %v, want a timeout", firstErr)
	}
	// Clearing a TCP deadline is normally recoverable. Here encryption has
	// already consumed a nonce the peer never received, so the carrier must end.
	_ = cc.SetWriteDeadline(time.Time{})
	if n, err := cc.Write([]byte("new record after the deadline is cleared")); n != 0 || !errors.Is(err, firstErr) {
		t.Fatalf("write after timeout = %d, %v; want original timeout", n, err)
	}
	_ = sc.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := sc.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("peer read after failed record = %v, want EOF for reconnect", err)
	}
}
