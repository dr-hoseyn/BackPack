package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const bulkMagic = "BP-BULK/1\n"
const bulkAck = "BP-BULK/1 OK\n"
const bulkBlock = 32 << 10

func bulkStream(seed []byte) cipher.Stream {
	block, _ := aes.NewCipher(seed[:32])
	return cipher.NewCTR(block, seed[32:48])
}

func bulkWrite(c net.Conn, payload []byte, timeout time.Duration, attempted *atomic.Uint64) error {
	stream := bulkStream(payload)
	buf := make([]byte, bulkBlock)
	for {
		clear(buf)
		stream.XORKeyStream(buf, buf)
		c.SetWriteDeadline(time.Now().Add(timeout))
		if attempted != nil {
			attempted.Add(uint64(len(buf)))
		}
		if _, err := io.Copy(c, bytes.NewReader(buf)); err != nil {
			return err
		}
	}
}

func bulkReceive(c net.Conn, payload []byte, timeout time.Duration, received *atomic.Uint64) error {
	buf := make([]byte, len(payload))
	expected := make([]byte, len(buf))
	stream := bulkStream(payload)
	for {
		c.SetReadDeadline(time.Now().Add(timeout))
		n, err := c.Read(buf)
		if n > 0 {
			clear(expected[:n])
			stream.XORKeyStream(expected[:n], expected[:n])
			if !bytes.Equal(buf[:n], expected[:n]) {
				return errors.New("bulk payload failed verification")
			}
			received.Add(uint64(n))
		}
		if err != nil {
			return err
		}
	}
}

// This explicit server mode avoids silently treating an old echo backend as a
// bulk receiver. Each session has a fresh random verification pattern. Upload
// counts come from receiver acknowledgements, never from the sender's buffers.
func serveBulk(c net.Conn) {
	limit := time.AfterFunc(2*time.Hour, func() { c.Close() })
	defer limit.Stop()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	head := make([]byte, len(bulkMagic)+1+bulkBlock)
	if _, err := io.ReadFull(c, head); err != nil || string(head[:len(bulkMagic)]) != bulkMagic {
		return
	}
	direction := head[len(bulkMagic)]
	if direction != 'U' && direction != 'D' {
		return
	}
	payload := head[len(bulkMagic)+1:]
	if _, err := io.WriteString(c, bulkAck); err != nil {
		return
	}
	var start [1]byte
	if _, err := io.ReadFull(c, start[:]); err != nil || start[0] != 1 {
		return
	}
	c.SetDeadline(time.Time{})
	if direction == 'D' {
		_ = bulkWrite(c, payload, 15*time.Second, nil)
		return
	}
	var received atomic.Uint64
	finished := make(chan error, 1)
	go func() { finished <- bulkReceive(c, payload, 15*time.Second, &received) }()
	defer func() { c.Close(); <-finished }()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var ack [8]byte
	for {
		select {
		case err := <-finished:
			// Put the completion back for the deferred join.
			finished <- err
			binary.BigEndian.PutUint64(ack[:], math.MaxUint64)
			c.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = c.Write(ack[:])
			return
		case <-tick.C:
			binary.BigEndian.PutUint64(ack[:], received.Load())
			c.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err := io.Copy(c, bytes.NewReader(ack[:])); err != nil {
				return
			}
		}
	}
}

func runBulkCase(parent context.Context, endpoint string, parallel int, duration, warmup, timeout time.Duration, direction byte, pids []int32) (result, error) {
	r := result{Endpoint: endpoint, Parallel: parallel, ChunkBytes: bulkBlock, IOTimeoutSeconds: timeout.Seconds(), Mode: "bulk", WarmupSeconds: warmup.Seconds()}
	if direction == 'U' {
		r.Direction = "upload"
	} else if direction == 'D' {
		r.Direction = "download"
	} else {
		return r, errors.New("invalid bulk direction")
	}
	if parallel < 1 || parallel > 256 || duration <= 0 || warmup < 0 || timeout <= 0 {
		return r, errors.New("invalid bulk bounds")
	}
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	conns := make([]net.Conn, parallel)
	defer func() {
		cancel()
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
		wg.Wait()
	}()
	patterns := make([][]byte, parallel)
	ready := make(chan error, parallel)
	var setup sync.WaitGroup
	for i := range parallel {
		setup.Go(func() {
			dialCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			c, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", endpoint)
			stop()
			if err != nil {
				ready <- err
				return
			}
			conns[i] = c
			context.AfterFunc(ctx, func() { c.Close() })
			c.SetDeadline(time.Now().Add(20 * time.Second))
			patterns[i] = make([]byte, bulkBlock)
			if _, err = rand.Read(patterns[i]); err != nil {
				ready <- err
				return
			}
			head := append(append([]byte(bulkMagic), direction), patterns[i]...)
			if _, err = io.Copy(c, bytes.NewReader(head)); err != nil {
				ready <- err
				return
			}
			ack := make([]byte, len(bulkAck))
			if _, err = io.ReadFull(c, ack); err != nil {
				ready <- err
				return
			}
			if string(ack) != bulkAck {
				ready <- errors.New("backend is not in bulk mode; run tunnelbench -serve ADDRESS -mode bulk")
				return
			}
			ready <- nil
		})
	}
	var setupErr error
	for range parallel {
		if err := <-ready; err != nil && setupErr == nil {
			setupErr = err
			cancel()
		}
	}
	setup.Wait()
	if setupErr != nil {
		return r, setupErr
	}
	counts := make([]atomic.Uint64, parallel)
	errorsCh := make(chan error, parallel)
	recordError := func(err error) {
		if ctx.Err() == nil {
			select {
			case errorsCh <- err:
			case <-ctx.Done():
			}
		}
	}
	for i, c := range conns {
		wg.Go(func() {
			c.SetDeadline(time.Time{})
			c.SetWriteDeadline(time.Now().Add(timeout))
			if _, err := c.Write([]byte{1}); err != nil {
				recordError(err)
				return
			}
			if direction == 'D' {
				recordError(bulkReceive(c, patterns[i], timeout, &counts[i]))
				return
			}
			var attempted atomic.Uint64
			writer := make(chan error, 1)
			go func() {
				err := bulkWrite(c, patterns[i], timeout, &attempted)
				recordError(err)
				c.Close()
				writer <- err
			}()
			defer func() { c.Close(); <-writer }()
			var ack [8]byte
			var previous uint64
			for {
				c.SetReadDeadline(time.Now().Add(timeout))
				if _, err := io.ReadFull(c, ack[:]); err != nil {
					recordError(err)
					return
				}
				n := binary.BigEndian.Uint64(ack[:])
				if n == math.MaxUint64 || n < previous || n > attempted.Load() {
					recordError(errors.New("invalid bulk upload verification acknowledgement"))
					return
				}
				counts[i].Store(n)
				previous = n
			}
		})
	}
	if warmup > 0 {
		timer := time.NewTimer(warmup)
		defer timer.Stop()
		select {
		case <-parent.Done():
			return r, parent.Err()
		case err := <-errorsCh:
			return r, err
		case <-timer.C:
		}
	}
	baseline := make([]uint64, parallel)
	for i := range baseline {
		baseline[i] = counts[i].Load()
	}
	var sm *sampler
	if len(pids) > 0 {
		sm = &sampler{roots: pids, last: map[string]float64{}, start: time.Now().UnixMilli()}
		sm.sample()
	}
	start := time.Now()
	r.StartedAt = start.UTC()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var runErr error
measurement:
	for {
		select {
		case <-parent.Done():
			runErr = parent.Err()
			break measurement
		case err := <-errorsCh:
			r.Failures++
			runErr = err
			break measurement
		case <-tick.C:
			if sm != nil {
				sm.sample()
			}
		case <-timer.C:
			break measurement
		}
	}
	r.Seconds = time.Since(start).Seconds()
	for i := range counts {
		n := counts[i].Load() - baseline[i]
		r.VerifiedBytes += n
		if n == 0 {
			r.Failures++
		}
	}
	// Sample before teardown so helper/connection cleanup is not charged to
	// the continuous-transfer CPU window.
	if sm != nil {
		sm.sample()
		sm.result.CPUPercent = sm.result.CPUSeconds / r.Seconds * 100
		r.Resources = &sm.result
	}
	cancel()
	wg.Wait()
	// A worker failure queued just before the deadline must not become success.
	if runErr == nil {
		select {
		case err := <-errorsCh:
			// Closing our sockets is expected after the measurement window.
			r.Failures++
			runErr = err
		default:
		}
	}
	if r.Seconds > 0 {
		r.BulkMbps = float64(r.VerifiedBytes) * 8 / r.Seconds / 1e6
	}
	if r.Failures > 0 && runErr == nil {
		runErr = errors.New("a bulk stream carried no verified traffic")
	}
	return r, runErr
}
