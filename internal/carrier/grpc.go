package carrier

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/http2"
)

const messageSize = 32 << 10

func servicePath(service string) string {
	if service == "" {
		service = "Backpack"
	}
	return "/" + service + "/Tun"
}

// Each carrier uses one HTTP/2 connection and one streaming gRPC call. The
// five-byte gRPC envelope preserves arbitrary tunnel bytes without protobuf.
type grpcConn struct {
	net.Conn
	reader   io.Reader
	writer   io.Writer
	flush    func() error
	closeFn  func()
	once     sync.Once
	rmu, wmu sync.Mutex
	left     uint32
	closed   bool // protected by wmu; ResponseWriter cannot outlive its handler
}

func (c *grpcConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for c.left == 0 {
		var h [5]byte
		if _, err := io.ReadFull(c.reader, h[:]); err != nil {
			return 0, err
		}
		c.left = binary.BigEndian.Uint32(h[1:])
		if h[0] != 0 || c.left > 1<<20 {
			return 0, fmt.Errorf("invalid gRPC message envelope")
		}
	}
	if uint32(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.reader.Read(p)
	c.left -= uint32(n)
	return n, err
}
func (c *grpcConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	total := 0
	for len(p) > 0 {
		n := min(len(p), messageSize)
		var h [5]byte
		binary.BigEndian.PutUint32(h[1:], uint32(n))
		if _, err := writeFull(c.writer, h[:]); err != nil {
			return total, err
		}
		m, err := writeFull(c.writer, p[:n])
		total += m
		if err != nil {
			return total, err
		}
		p = p[n:]
		if c.flush != nil {
			if err := c.flush(); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}
func writeFull(w io.Writer, p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := w.Write(p)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
func (c *grpcConn) Close() error {
	c.once.Do(func() {
		// Closing the socket first releases a writer blocked by flow control.
		c.Conn.Close()
		c.wmu.Lock()
		defer c.wmu.Unlock()
		c.closed = true
		c.closeFn()
	})
	return nil
}

func grpcClient(ctx context.Context, raw net.Conn, host, service string) (net.Conn, error) {
	tr := &http2.Transport{}
	cc, err := tr.NewClientConn(raw)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	// Lifetime belongs to the carrier, rather than the timeout used only to
	// establish it. Closing raw or the pipes cancels an outstanding call.
	req, err := http.NewRequest("POST", "https://"+host+servicePath(service), pr)
	if err != nil {
		pr.Close()
		pw.Close()
		cc.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	type answer struct {
		resp *http.Response
		err  error
	}
	ready := make(chan answer, 1)
	go func() { r, e := cc.RoundTrip(req); ready <- answer{r, e} }()
	select {
	case a := <-ready:
		if a.err != nil {
			pr.Close()
			pw.Close()
			cc.Close()
			return nil, a.err
		}
		if a.resp.StatusCode != http.StatusOK || !strings.HasPrefix(a.resp.Header.Get("Content-Type"), "application/grpc") {
			a.resp.Body.Close()
			pr.Close()
			pw.Close()
			cc.Close()
			return nil, fmt.Errorf("gRPC endpoint returned HTTP %d", a.resp.StatusCode)
		}
		return &grpcConn{Conn: raw, reader: a.resp.Body, writer: pw, closeFn: func() { pr.Close(); pw.Close(); a.resp.Body.Close(); cc.Close() }}, nil
	case <-ctx.Done():
		pr.Close()
		pw.Close()
		cc.Close()
		return nil, ctx.Err()
	}
}

func grpcServer(ctx context.Context, raw net.Conn, service string) (net.Conn, error) {
	ready := make(chan net.Conn, 1)
	stopped := make(chan struct{})
	var used sync.Once
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != servicePath(service) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			http.NotFound(w, r)
			return
		}
		accepted := false
		used.Do(func() { accepted = true })
		if !accepted {
			http.Error(w, "one carrier per connection", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		conn := &grpcConn{Conn: raw, reader: r.Body, writer: w, flush: rc.Flush, closeFn: func() { close(done); r.Body.Close() }}
		select {
		case ready <- conn:
		case <-ctx.Done():
			conn.Close()
			return
		}
		select {
		case <-done:
		case <-r.Context().Done():
			conn.Close()
		case <-ctx.Done():
			conn.Close()
		}
		w.Header().Set("Grpc-Status", "0")
	})
	server := &http2.Server{MaxConcurrentStreams: 1, MaxUploadBufferPerConnection: 8 << 20, MaxUploadBufferPerStream: 4 << 20}
	go func() {
		defer close(stopped)
		server.ServeConn(raw, &http2.ServeConnOpts{Context: ctx, Handler: handler})
	}()
	select {
	case conn := <-ready:
		return conn, nil
	case <-stopped:
		return nil, io.EOF
	case <-ctx.Done():
		raw.Close()
		return nil, ctx.Err()
	}
}
