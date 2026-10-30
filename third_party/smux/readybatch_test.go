package smux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

type batchTestWriter struct {
	calls   chan []byte
	entered chan struct{}
	release chan struct{}
	partial int
	err     error
}

func (w *batchTestWriter) WriteBufferBatchLimits() (int, int) { return 16, 65536 }
func (w *batchTestWriter) WriteBuffers(v [][]byte) (int, error) {
	if w.entered != nil {
		close(w.entered)
		<-w.release
	}
	data := bytes.Join(v, nil)
	if w.calls != nil {
		w.calls <- data
	}
	if w.partial >= 0 {
		return w.partial, w.err
	}
	return len(data), w.err
}

func batchRequest(id uint32, payload []byte, class CLASSID) writeRequest {
	f := newFrame(2, cmdPSH, id)
	f.data = payload
	return writeRequest{class: class, frame: f, seq: id, result: make(chan writeResult, 1)}
}

func startBatchSender(t *testing.T, requests []writeRequest, writer readyBatchWriter, frames, size int) *Session {
	t.Helper()
	s := &Session{sq: NewShaperQueue(), die: make(chan struct{}), chShaperPending: make(chan struct{}, 1),
		chShaperConsumed: make(chan struct{}, 1), chSocketWriteError: make(chan struct{})}
	for _, r := range requests {
		s.sq.Push(r)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.sendReadyBatches(writer, frames, size) }()
	s.notifyShaperPending()
	t.Cleanup(func() {
		close(s.die)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("batch sender did not stop")
		}
	})
	return s
}

func batchResult(t *testing.T, r writeRequest) writeResult {
	t.Helper()
	select {
	case got := <-r.result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("request not completed")
		return writeResult{}
	}
}

func TestReadyBatchFlushesSingleWriteWithoutWaitingForAnother(t *testing.T) {
	r := batchRequest(1, []byte("interactive"), CLSDATA)
	w := &batchTestWriter{calls: make(chan []byte, 1), partial: -1}
	startBatchSender(t, []writeRequest{r}, w, 16, 65536)
	got := batchResult(t, r)
	if got.err != nil || got.n != len(r.frame.data) {
		t.Fatal(got)
	}
	if !bytes.Equal((<-w.calls)[headerSize:], r.frame.data) {
		t.Fatal("payload changed")
	}
}

func TestReadyBatchBoundedOrderAndControls(t *testing.T) {
	for _, scenario := range []string{"frame-limit", "byte-limit", "control", "fin", "maximum-frame"} {
		t.Run(scenario, func(t *testing.T) {
			var requests []writeRequest
			frames, size, expected := 2, 65536, 2
			for i := 1; i <= 3; i++ {
				requests = append(requests, batchRequest(uint32(i), bytes.Repeat([]byte{byte(i)}, 8), CLSDATA))
			}
			switch scenario {
			case "byte-limit":
				size, expected = 24, 3
			case "control":
				requests[1].class = CLSCTRL
				requests[1].frame.cmd = cmdNOP
				expected = 3
			case "fin":
				requests[1].frame.cmd = cmdFIN
				expected = 3
			case "maximum-frame":
				requests[0].frame.data = make([]byte, 65535)
			}
			w := &batchTestWriter{calls: make(chan []byte, 4), partial: -1}
			startBatchSender(t, requests, w, frames, size)
			for _, r := range requests {
				if got := batchResult(t, r); got.err != nil || got.n != len(r.frame.data) {
					t.Fatal(got)
				}
			}
			if len(w.calls) != expected {
				t.Fatalf("writes=%d want=%d", len(w.calls), expected)
			}
			var ids []uint32
			for len(w.calls) > 0 {
				data := <-w.calls
				if len(data) > size && len(data) != 65535+headerSize {
					t.Fatal("batch exceeded byte limit")
				}
				for len(data) > 0 {
					length := int(binary.LittleEndian.Uint16(data[2:4]))
					ids = append(ids, binary.LittleEndian.Uint32(data[4:8]))
					data = data[headerSize+length:]
				}
			}
			if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
				t.Fatalf("round-robin order changed: %v", ids)
			}
		})
	}
}

func TestReadyBatchCompletesAfterBorrowedBuffersAreConsumed(t *testing.T) {
	r := batchRequest(1, []byte("borrowed"), CLSDATA)
	w := &batchTestWriter{entered: make(chan struct{}), release: make(chan struct{}), partial: -1}
	done := make(chan error, 1)
	go func() { done <- writeReadyBatch(w, []writeRequest{r}, make([]byte, 8), make([][]byte, 2)) }()
	<-w.entered
	select {
	case <-r.result:
		t.Error("request completed before synchronous writer consumed the payload")
	default:
	}
	close(w.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := batchResult(t, r); got.n != len(r.frame.data) || got.err != nil {
		t.Fatal(got)
	}
}

func TestReadyBatchPartialErrorsAccountForEachPayload(t *testing.T) {
	failure := errors.New("socket failed")
	for _, count := range []int{0, 4, 8, 11, 15, 19, 22} {
		t.Run(string(rune('A'+count)), func(t *testing.T) {
			a, b := batchRequest(1, []byte("abc"), CLSDATA), batchRequest(2, []byte("xyz"), CLSDATA)
			w := &batchTestWriter{partial: count, err: failure}
			if err := writeReadyBatch(w, []writeRequest{a, b}, make([]byte, 16), make([][]byte, 4)); err != failure {
				t.Fatal(err)
			}
			left, right := batchResult(t, a), batchResult(t, b)
			clamp := func(n int) int {
				if n < 0 {
					return 0
				}
				if n > 3 {
					return 3
				}
				return n
			}
			wantA, wantB := clamp(count-8), clamp(count-19)
			if left.n != wantA || right.n != wantB || right.err != failure || left.err != failure {
				t.Fatalf("written=%d results=%+v %+v", count, left, right)
			}
		})
	}
	a := batchRequest(1, []byte("abc"), CLSDATA)
	if err := writeReadyBatch(&batchTestWriter{partial: 9}, []writeRequest{a}, make([]byte, 8), make([][]byte, 2)); err != io.ErrShortWrite {
		t.Fatalf("nil short-write error: %v", err)
	}
}
