// Copyright (c) 2026 BackPack contributors.
// SPDX-License-Identifier: MIT

package smux

import (
	"encoding/binary"
	"io"
)

// Opt-in extension: ordinary upstream connections keep the original sendLoop.
// WriteBuffers must consume all borrowed vectors synchronously, as before.
type readyBatchWriter interface {
	WriteBuffers([][]byte) (int, error)
	WriteBufferBatchLimits() (frames, bytes int)
}

func (s *Session) sendReadyBatches(writer readyBatchWriter, maxFrames, maxBytes int) {
	requests := make([]writeRequest, maxFrames)
	headers := make([]byte, maxFrames*headerSize)
	vectors := make([][]byte, maxFrames*2)
	for {
		select {
		case <-s.die:
			return
		case <-s.chShaperPending:
		}
		for {
			first, ok := s.sq.Pop()
			if !ok {
				s.notifyShaperConsumed()
				break
			}
			requests[0] = first
			count, size := 1, headerSize+len(first.frame.data)
			// Never wait for another request. Controls and a maximum-size
			// frame leave on their own; data already queued stays round-robin.
			for first.class == CLSDATA && first.frame.cmd == cmdPSH && count < maxFrames && size < maxBytes {
				next, ok := s.sq.popLimited(maxBytes-size, true)
				if !ok {
					break
				}
				requests[count] = next
				count++
				size += headerSize + len(next.frame.data)
			}
			err := writeReadyBatch(writer, requests[:count], headers[:count*headerSize], vectors[:count*2])
			// An idle physical session must not retain old callers' payloads.
			for i := 0; i < count; i++ {
				requests[i] = writeRequest{}
				vectors[i*2], vectors[i*2+1] = nil, nil
			}
			if err != nil {
				s.notifyWriteError(err)
				return
			}
		}
	}
}

// Report per-frame results only after the underlying synchronous write ends.
// A writer may buffer bytes before failing during flush. Conservatively pass
// its error to every request; never count a header as application bytes.
func writeReadyBatch(writer readyBatchWriter, requests []writeRequest, headers []byte, vectors [][]byte) error {
	total := 0
	for i, request := range requests {
		h := headers[i*headerSize : (i+1)*headerSize]
		h[0], h[1] = request.frame.ver, request.frame.cmd
		binary.LittleEndian.PutUint16(h[2:], uint16(len(request.frame.data)))
		binary.LittleEndian.PutUint32(h[4:], request.frame.sid)
		vectors[i*2], vectors[i*2+1] = h, request.frame.data
		total += headerSize + len(request.frame.data)
	}
	n, err := writer.WriteBuffers(vectors)
	if n < 0 || n > total || (n < total && err == nil) {
		err = io.ErrShortWrite
	}
	if n < 0 {
		n = 0
	}
	if n > total {
		n = total
	}
	remaining := n
	for _, request := range requests {
		wireSize := headerSize + len(request.frame.data)
		written := remaining
		if written > wireSize {
			written = wireSize
		}
		payload := written - headerSize
		if payload < 0 {
			payload = 0
		}
		request.result <- writeResult{n: payload, err: err}
		remaining -= written
	}
	return err
}
