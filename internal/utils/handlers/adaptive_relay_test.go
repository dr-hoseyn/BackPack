package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"slices"
	"testing"
)

func TestAdaptiveRelayFramedReadsPreservePayloadAndGrowth(t *testing.T) {
	for _, frame := range []int{smallRelayBufferSize - 16, 16368, 32752, 65520} {
		for _, size := range []int{1, smallRelayBufferSize - 1, smallRelayBufferSize, 3*relayBufferSize + 17} {
			t.Run(fmt.Sprintf("frame-%d/bytes-%d", frame, size), func(t *testing.T) {
				payload := bytes.Repeat([]byte{byte(frame)}, size)
				from := &sizedReadConn{data: payload}
				to := &completedWriteConn{}
				buffer := newRelayCopyBuffer()
				buffer.frameSize = frame
				if !copyBuffered(from, to, &buffer, quietLogger(), nil, 0, false) {
					t.Fatal("framed copy lost buffer ownership")
				}
				defer buffer.release()
				if !bytes.Equal(to.received, payload) {
					t.Fatal("framed read lost a tail or changed payload")
				}
				first := smallRelayBufferSize
				if first >= frame {
					first = first / frame * frame
				}
				if from.sizes[0] != first {
					t.Fatal("initial read ignored the destination frame")
				}
				if size >= first && from.sizes[1] != relayBufferSize/frame*frame {
					t.Fatal("full read did not grow to whole bulk frames")
				}
				if size < first && len(*buffer.p) != smallRelayBufferSize {
					t.Fatal("short response acquired a bulk buffer")
				}
			})
		}
	}
}

func TestAdaptiveRelayFramedFailedWriteKeepsQueuedBytes(t *testing.T) {
	// Exercise alignment in the initial buffer as well as retained ownership.
	const frame = smallRelayBufferSize - 16
	from := &sizedReadConn{data: bytes.Repeat([]byte("q"), frame+1)}
	to := &queuedWriteConn{err: net.ErrClosed}
	buffer := newRelayCopyBuffer()
	buffer.frameSize = frame
	if copyBuffered(from, to, &buffer, quietLogger(), nil, 0, false) {
		t.Fatal("framed failed write allowed recycling its queued slice")
	}
	if len(to.queued) != frame || !bytes.Equal(to.queued, bytes.Repeat([]byte("q"), frame)) {
		t.Fatal("framed failed write changed its outstanding bytes")
	}
}

type sizedReadConn struct {
	net.Conn
	data  []byte
	sizes []int
}

func (c *sizedReadConn) Read(p []byte) (int, error) {
	c.sizes = append(c.sizes, len(p))
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, nil
}
func (*sizedReadConn) Close() error { return nil }

func TestAdaptiveRelayPreservesPayloadAndGrowsOnlyForFullReads(t *testing.T) {
	for _, size := range []int{0, 1, smallRelayBufferSize - 1, smallRelayBufferSize, 3 * relayBufferSize} {
		payload := bytes.Repeat([]byte{0x39}, size)
		from := &sizedReadConn{data: payload}
		to := &completedWriteConn{}
		buffer := newRelayCopyBuffer()
		if !copyBuffered(from, to, &buffer, quietLogger(), nil, 0, false) {
			t.Fatal("completed transfer lost buffer ownership")
		}
		if !bytes.Equal(payload, to.received) {
			t.Fatalf("payload corrupted at size %d", size)
		}
		if from.sizes[0] != smallRelayBufferSize {
			t.Fatalf("quiet direction starts with %d bytes", from.sizes[0])
		}
		if size < smallRelayBufferSize && slices.Contains(from.sizes, relayBufferSize) {
			t.Fatal("short transfer acquired a bulk buffer")
		}
		if size >= smallRelayBufferSize && from.sizes[1] != relayBufferSize {
			t.Fatal("full read did not immediately grow for bulk transfer")
		}
		buffer.release()
	}
}

func TestAdaptiveRelayFailedWriteDoesNotRecycleQueuedBytes(t *testing.T) {
	for _, size := range []int{32, 3 * smallRelayBufferSize} {
		from := &sizedReadConn{data: bytes.Repeat([]byte{'q'}, size)}
		to := &queuedWriteConn{err: net.ErrClosed}
		buffer := newRelayCopyBuffer()
		if copyBuffered(from, to, &buffer, quietLogger(), nil, 0, false) {
			t.Fatal("failed smux write allowed reuse of its outstanding slice")
		}
		if len(to.queued) != min(size, smallRelayBufferSize) || !bytes.Equal(to.queued, bytes.Repeat([]byte{'q'}, len(to.queued))) {
			t.Fatal("outstanding payload changed")
		}
	}
}

func TestAdaptiveRelayWarmPoolDoesNotAllocate(t *testing.T) {
	if raceEnabled {
		// sync.Pool deliberately drops 1/4 of Put calls under -race. Two
		// pools therefore measure injected cold allocations, not reuse cost.
		t.Skip("allocation budget runs without race-injected pool discards")
	}
	b := newRelayCopyBuffer()
	b.grow(smallRelayBufferSize)
	b.release()
	if got := testing.AllocsPerRun(500, func() {
		b := newRelayCopyBuffer()
		b.grow(smallRelayBufferSize)
		b.release()
	}); got != 0 {
		t.Fatalf("warm adaptive buffer allocated %.0f times", got)
	}
}

func BenchmarkAdaptiveRelayBufferPair(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		up, down := newRelayCopyBuffer(), newRelayCopyBuffer()
		up.release()
		down.release()
	}
}
