package handlers

import (
	"bytes"
	"io"
	"net"
	"slices"
	"testing"
)

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
