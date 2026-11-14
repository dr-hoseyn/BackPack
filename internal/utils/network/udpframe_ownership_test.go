package network

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// smux can release a timed-out Write while its send loop still holds the
// frame. A vector writer keeps that ownership visible until explicitly drained.
type datagramVectorSink struct {
	started  chan struct{}
	release  chan struct{}
	received chan []byte
	closed   chan struct{}
	once     sync.Once
}

func (s *datagramVectorSink) Read([]byte) (int, error) {
	<-s.closed
	return 0, io.EOF
}

func (s *datagramVectorSink) Write(p []byte) (int, error) { return len(p), nil }

func (s *datagramVectorSink) WriteBuffers(v [][]byte) (int, error) {
	n := len(v[0]) + len(v[1])
	if len(v[1]) == 0 {
		return n, nil // stream open/close control frames
	}
	close(s.started)
	select {
	case <-s.release:
		s.received <- bytes.Clone(v[1])
		return n, nil
	case <-s.closed:
		return 0, io.ErrClosedPipe
	}
}

func (s *datagramVectorSink) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestTimedOutDatagramKeepsItsQueuedMuxFrameImmutable(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("smux-v%d", version), func(t *testing.T) {
			timedOutDatagramKeepsItsQueuedMuxFrameImmutable(t, version)
		})
	}
}

func timedOutDatagramKeepsItsQueuedMuxFrameImmutable(t *testing.T, version int) {
	t.Helper()
	sink := &datagramVectorSink{
		started: make(chan struct{}), release: make(chan struct{}),
		received: make(chan []byte, 1), closed: make(chan struct{}),
	}
	cfg := smux.DefaultConfig()
	cfg.Version = version
	cfg.KeepAliveDisabled = true
	session, err := smux.Client(sink, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stream, err := session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("UDP reply still owned by the mux send loop")
	done := make(chan error, 1)
	go func() { done <- WriteDatagram(stream, payload) }()
	select {
	case <-sink.started:
	case <-time.After(3 * time.Second):
		t.Fatal("mux did not start writing the datagram")
	}
	if err := stream.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled datagram write did not time out")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream deadline did not interrupt the datagram write")
	}

	// Exhaust the bounded size class, just as other flows do while this
	// session's send loop is stalled. None may overwrite the queued frame.
	borrowed := make([][]byte, cap(datagramBuffers[0])+1)
	for i := range borrowed {
		borrowed[i] = GetDatagramBuffer(2 + len(payload))
		for j := range borrowed[i] {
			borrowed[i][j] = 0xee
		}
	}
	defer func() {
		for _, b := range borrowed {
			PutDatagramBuffer(b)
		}
	}()
	close(sink.release)
	select {
	case frame := <-sink.received:
		got, err := ReadDatagramInto(bytes.NewReader(frame), nil)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("timed-out queued datagram corrupted: payload=%q, error=%v", got, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mux did not finish its queued frame")
	}
}

func TestCompletedDatagramsKeepAllocationFreeBufferReuse(t *testing.T) {
	payload := bytes.Repeat([]byte{7}, 1200)
	if allocations := testing.AllocsPerRun(1000, func() {
		if err := WriteDatagram(io.Discard, payload); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("completed datagram: %.1f allocations, want none after warmup", allocations)
	}
}

type shortDatagramWriter struct{ bytes.Buffer }

func (w *shortDatagramWriter) Write(p []byte) (int, error) {
	return w.Buffer.Write(p[:min(3, len(p))])
}

func TestShortDatagramWritesStillDeliverACompleteFrame(t *testing.T) {
	payload := []byte("datagram written in small pieces")
	var writer shortDatagramWriter
	if err := WriteDatagram(&writer, payload); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDatagramInto(&writer.Buffer, nil)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("short writes corrupted datagram: payload=%q, error=%v", got, err)
	}
}
