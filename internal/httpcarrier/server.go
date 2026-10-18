package httpcarrier

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"golang.org/x/net/http2"
)

type Server struct {
	ctx                          context.Context
	cancel                       context.CancelFunc
	protocol, path, auth, target string
	h2                           *http.Server
	h3                           *http3.Server
	wt                           *webtransport.Server
	tcp                          net.Listener
	udp                          net.PacketConn
	wg                           sync.WaitGroup
	once                         sync.Once
	streams                      chan struct{}
	workers                      sync.WaitGroup
	mu                           sync.Mutex
}

func Listen(parent context.Context, protocol, mode, addr, target, user, password, path string, cfg *tls.Config) (*Server, error) {
	if err := Validate(protocol, mode, user, password, path); err != nil {
		return nil, err
	}
	if err := FixedTarget(target); err != nil {
		return nil, err
	}
	t, err := verifiedTLS(cfg)
	if err != nil {
		return nil, err
	}
	if len(t.Certificates) == 0 {
		return nil, errors.New("HTTP carrier server requires a certificate")
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Server{ctx: ctx, cancel: cancel, protocol: protocol, path: path, auth: authorization(user, password), target: target, streams: make(chan struct{}, 256)}
	// Reserve both sockets before serving. Failure rolls back every listener.
	if protocol == "trusttunnel" && mode != "h3" {
		tc := t.Clone()
		tc.NextProtos = []string{"h2"}
		s.tcp, err = tls.Listen("tcp", addr, tc)
		if err != nil {
			cancel()
			return nil, err
		}
		addr = s.tcp.Addr().String()
	}
	if protocol == "webtransport" || mode != "h2" {
		s.udp, err = net.ListenPacket("udp", addr)
		if err != nil {
			if s.tcp != nil {
				s.tcp.Close()
			}
			cancel()
			return nil, err
		}
		s.h3 = &http3.Server{TLSConfig: http3.ConfigureTLSConfig(t), QUICConfig: quicConfig(), MaxHeaderBytes: 8192}
		if protocol == "webtransport" {
			webtransport.ConfigureHTTP3Server(s.h3)
			s.wt = &webtransport.Server{H3: s.h3, ReorderingTimeout: 5 * time.Second, ApplicationProtocols: []string{"backpack-stream-v1"}}
		}
		s.h3.Handler = http.HandlerFunc(s.handle)
	}
	if s.tcp != nil {
		s.h2 = &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
		if err = http2.ConfigureServer(s.h2, &http2.Server{MaxConcurrentStreams: 128}); err != nil {
			s.Close()
			return nil, err
		}
		s.wg.Go(func() { _ = s.h2.Serve(s.tcp); s.cancel() })
	}
	if s.udp != nil {
		s.wg.Go(func() {
			if s.wt != nil {
				_ = s.wt.Serve(s.udp)
			} else {
				_ = s.h3.Serve(s.udp)
			}
			s.cancel()
		})
	}
	context.AfterFunc(ctx, func() { s.Close() })
	return s, nil
}

func (s *Server) Addr() net.Addr {
	if s.tcp != nil {
		return s.tcp.Addr()
	}
	return s.udp.LocalAddr()
}
func (s *Server) Done() <-chan struct{} { return s.ctx.Done() }
func (s *Server) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.cancel()
		s.mu.Unlock()
		if s.h2 != nil {
			s.h2.Close()
		}
		if s.wt != nil {
			s.wt.Close()
		} else if s.h3 != nil {
			s.h3.Close()
		}
		if s.tcp != nil {
			s.tcp.Close()
		}
		if s.udp != nil {
			s.udp.Close()
		}
	})
	s.wg.Wait()
	s.workers.Wait()
	return nil
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	s.workers.Add(1)
	s.mu.Unlock()
	defer s.workers.Done()
	header := "Proxy-Authorization"
	if s.protocol == "webtransport" {
		header = "Authorization"
	}
	if !authorized(r.Header.Get(header), s.auth) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="backpack"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return
	}
	if r.Method != http.MethodConnect {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if s.protocol == "webtransport" {
		if r.URL.Path != s.path || r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sess, err := s.wt.Upgrade(w, r)
		if err != nil {
			return
		}
		var workers sync.WaitGroup
		defer func() { sess.CloseWithError(0, ""); workers.Wait() }()
		if sess.SessionState().ApplicationProtocol != "backpack-stream-v1" {
			return
		}
		for {
			stream, err := sess.AcceptStream(s.ctx)
			if err != nil {
				return
			}
			select {
			case s.streams <- struct{}{}:
			default:
				stream.CancelRead(0)
				stream.CancelWrite(0)
				continue
			}
			workers.Go(func() {
				defer func() { <-s.streams }()
				conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(sess.Context(), "tcp", s.target)
				if err != nil {
					stream.CancelRead(0)
					stream.CancelWrite(0)
					return
				}
				Relay(sess.Context(), conn, &webStream{Stream: stream})
			})
		}
	}
	if r.Host != s.target || r.URL.Path != "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(r.Context(), "tcp", s.target)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(r.Context(), func() { conn.Close() })
	defer stop()
	w.WriteHeader(http.StatusOK)
	flush := http.NewResponseController(w)
	if err := flush.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := io.Copy(conn, r.Body)
		if err == nil {
			if tcp, ok := conn.(*net.TCPConn); ok {
				tcp.CloseWrite()
				return
			}
		}
		conn.Close()
	}()
	_, _ = io.Copy(flushWriter{w}, conn)
	conn.Close()
	r.Body.Close()
	<-done
}

type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}
