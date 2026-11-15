// Package bridge joins two connections: the last step of both the direct and
// layer-3 engines' port forwarding.
package bridge

import (
	"context"
	"io"
	"net"

	"github.com/backpack/backpack/internal/metrics"
)

// Join copies both directions, preserving directional EOF until the response drains.
// Streams without directional EOF retain their existing full-close behavior.
func Join(ctx context.Context, a, b net.Conn) {
	// Reuse the caller for one copy. Cancellation must interrupt that copy
	// even while it is blocked in Read, before it reaches the join loop.
	stop := context.AfterFunc(ctx, func() { a.Close(); b.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	aWrite, bWrite := halfCloseWriter(a), halfCloseWriter(b)
	copyOne := func(dst, src net.Conn, write writeHalfCloser) {
		defer func() { done <- struct{}{} }()
		_, err := io.Copy(dst, src)
		if err == nil && aWrite != nil && bWrite != nil && write.CloseWrite() == nil {
			return
		}
		a.Close()
		b.Close()
	}
	go copyOne(a, b, aWrite)
	copyOne(b, a, bWrite)

	finished := 0
	for finished < 2 {
		select {
		case <-done:
			finished++
		case <-ctx.Done():
			a.Close()
			b.Close()
			for finished < 2 {
				<-done
				finished++
			}
		}
	}
	a.Close()
	b.Close()
}

type writeHalfCloser interface {
	CloseWrite() error
}

// Unwrap only to send FIN; copying through the original wrappers keeps pacing
// and traffic accounting active in both directions.
func halfCloseWriter(conn net.Conn) writeHalfCloser {
	for range 8 {
		// Mux streams provide directional EOF too. Restricting this to raw
		// TCP sockets discards the response when a Direct user ends an upload.
		if write, ok := conn.(writeHalfCloser); ok {
			return write
		}
		raw, counted := metrics.Uncount(conn)
		if counted {
			conn = raw
			continue
		}
		if wrapper, ok := conn.(interface{ UnderlyingConn() net.Conn }); ok {
			conn = wrapper.UnderlyingConn()
			continue
		}
		return nil
	}
	return nil
}
