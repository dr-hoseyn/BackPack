package masqueh2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

const window = 1 << 20

// One CONNECT stream per TLS connection keeps flow ownership explicit. HTTP/2
// framing and HPACK parsing use x/net; this endpoint neither changes x/net's
// process-wide Extended CONNECT switch nor enables it for unrelated web panels.
type session struct {
	conn           net.Conn
	fr             *http2.Framer
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	once           sync.Once
	writeMu        sync.Mutex
	sendMu         sync.Mutex
	mu             sync.Mutex
	connWindow     int64
	streamWindow   int64
	initialWindow  int64
	wake           chan struct{}
	ready          chan error
	server         bool
	opened         bool
	settings       bool
	connectAllowed bool
	header         []hpack.HeaderField
	validate       func([]hpack.HeaderField) error
	deliver        func([]byte)
	decode         decoder
	credit         chan struct{}
	received       atomic.Uint32
	terminal       error
}

func newSession(parent context.Context, conn net.Conn, server bool, headers []hpack.HeaderField, validate func([]hpack.HeaderField) error, deliver func([]byte)) *session {
	ctx, cancel := context.WithCancel(parent)
	s := &session{conn: conn, ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}), ready: make(chan error, 1), credit: make(chan struct{}, 1), server: server, header: headers, validate: validate, deliver: deliver,
		connWindow: 65535, streamWindow: 65535, initialWindow: 65535}
	s.fr = http2.NewFramer(conn, conn)
	s.fr.SetMaxReadFrameSize(16384)
	s.fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	s.fr.MaxHeaderListSize = 8192
	return s
}

func (s *session) close() {
	s.once.Do(func() {
		s.cancel()
		s.conn.Close()
		s.mu.Lock()
		close(s.wake)
		s.wake = make(chan struct{})
		s.mu.Unlock()
	})
}

func (s *session) write(fn func() error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return net.ErrClosed
	}
	if err := s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	err := fn()
	if err != nil {
		s.close()
	}
	return err
}

func (s *session) headers(fields []hpack.HeaderField) error {
	var b bytes.Buffer
	encoder := hpack.NewEncoder(&b)
	for _, f := range fields {
		if err := encoder.WriteField(f); err != nil {
			return err
		}
	}
	return s.write(func() error {
		return s.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: b.Bytes(), EndHeaders: true})
	})
}

func (s *session) run() {
	defer close(s.done)
	defer s.close()
	stop := context.AfterFunc(s.ctx, func() { s.conn.Close() })
	defer stop()
	if err := s.conn.SetDeadline(time.Now().Add(12 * time.Second)); err != nil {
		s.signal(err)
		return
	}
	if s.server {
		b := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(s.conn, b); err != nil || string(b) != http2.ClientPreface {
			s.signal(errors.New("invalid HTTP/2 preface"))
			return
		}
	} else if _, err := io.WriteString(s.conn, http2.ClientPreface); err != nil {
		s.signal(err)
		return
	}
	if err := s.write(func() error {
		if err := s.fr.WriteSettings(http2.Setting{ID: http2.SettingEnableConnectProtocol, Val: 1}, http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 1}, http2.Setting{ID: http2.SettingInitialWindowSize, Val: window}, http2.Setting{ID: http2.SettingMaxHeaderListSize, Val: 8192}); err != nil {
			return err
		}
		return s.fr.WriteWindowUpdate(0, window-65535)
	}); err != nil {
		s.signal(err)
		return
	}
	creditDone := make(chan struct{})
	go func() {
		defer close(creditDone)
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-s.credit:
				n := s.received.Swap(0)
				if n != 0 {
					if err := s.write(func() error {
						if err := s.fr.WriteWindowUpdate(0, n); err != nil {
							return err
						}
						return s.fr.WriteWindowUpdate(1, n)
					}); err != nil {
						s.signal(err)
						return
					}
				}
			}
		}
	}()
	defer func() { s.close(); <-creditDone }()
	for {
		frame, err := s.fr.ReadFrame()
		if err != nil {
			s.signal(err)
			return
		}
		if !s.settings {
			f, ok := frame.(*http2.SettingsFrame)
			if !ok || f.IsAck() {
				s.signal(errors.New("first HTTP/2 frame must be SETTINGS"))
				return
			}
		}
		if err := s.handle(frame); err != nil {
			s.signal(err)
			return
		}
	}
}

func (s *session) signal(err error) {
	if err != nil {
		s.mu.Lock()
		if s.terminal == nil {
			s.terminal = err
		}
		s.mu.Unlock()
	}
	select {
	case s.ready <- err:
	default:
	}
}

func (s *session) handle(frame http2.Frame) error {
	switch f := frame.(type) {
	case *http2.SettingsFrame:
		if f.IsAck() {
			return nil
		}
		if err := f.ForeachSetting(func(setting http2.Setting) error {
			if err := setting.Valid(); err != nil {
				return err
			}
			switch setting.ID {
			case http2.SettingInitialWindowSize:
				s.mu.Lock()
				s.streamWindow += int64(setting.Val) - s.initialWindow
				s.initialWindow = int64(setting.Val)
				close(s.wake)
				s.wake = make(chan struct{})
				s.mu.Unlock()
			case http2.SettingEnableConnectProtocol:
				s.connectAllowed = setting.Val == 1
			}
			return nil
		}); err != nil {
			return err
		}
		first := !s.settings
		s.settings = true
		if err := s.write(s.fr.WriteSettingsAck); err != nil {
			return err
		}
		if first && !s.server {
			if !s.connectAllowed {
				return errors.New("peer does not support HTTP/2 Extended CONNECT")
			}
			return s.headers(s.header)
		}
	case *http2.MetaHeadersFrame:
		if f.StreamID != 1 || s.opened || f.Truncated || f.StreamEnded() {
			return errors.New("invalid CONNECT stream")
		}
		if s.server {
			if err := s.validate(f.Fields); err != nil {
				_ = s.headers([]hpack.HeaderField{{Name: ":status", Value: "403"}})
				return err
			}
			if err := s.headers([]hpack.HeaderField{{Name: ":status", Value: "200"}, {Name: "capsule-protocol", Value: "?1"}}); err != nil {
				return err
			}
		} else {
			status, capsule := "", ""
			for _, h := range f.Fields {
				if h.Name == ":status" {
					status = h.Value
				}
				if h.Name == "capsule-protocol" {
					capsule = h.Value
				}
			}
			code, _ := strconv.Atoi(status)
			if code < 200 || code > 299 || capsule != "?1" {
				return fmt.Errorf("CONNECT-UDP refused (HTTP %s)", status)
			}
		}
		s.opened = true
		if err := s.conn.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
		s.signal(nil)
	case *http2.DataFrame:
		if !s.opened || f.StreamID != 1 || f.StreamEnded() {
			return errors.New("data outside CONNECT stream")
		}
		if err := s.decode.feed(f.Data(), s.deliver); err != nil {
			return err
		}
		if f.Length != 0 {
			s.received.Add(f.Length)
			select {
			case s.credit <- struct{}{}:
			default:
			}
		}
	case *http2.WindowUpdateFrame:
		if f.StreamID != 0 && f.StreamID != 1 {
			return errors.New("unknown stream window")
		}
		s.mu.Lock()
		w := &s.connWindow
		if f.StreamID == 1 {
			w = &s.streamWindow
		}
		*w += int64(f.Increment)
		overflow := *w > 1<<31-1
		close(s.wake)
		s.wake = make(chan struct{})
		s.mu.Unlock()
		if overflow {
			return errors.New("HTTP/2 flow window overflow")
		}
	case *http2.PingFrame:
		if !f.IsAck() {
			return s.write(func() error { return s.fr.WritePing(true, f.Data) })
		}
	case *http2.GoAwayFrame, *http2.RSTStreamFrame:
		return io.EOF
	case *http2.PushPromiseFrame:
		return errors.New("server push is not supported")
	}
	return nil
}

// send serializes complete Capsules, including when the peer's flow window
// splits one across DATA frames. A cancelled or timed-out send closes the
// connection: a partial capsule cannot be replaced with a new packet.
func (s *session) send(ctx context.Context, p []byte) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if len(p) > maxPacket {
		return errors.New("MASQUE datagram exceeds UDP payload limit")
	}
	b := datagramCapsule(p)
	for len(b) != 0 {
		s.mu.Lock()
		n := int(min(int64(len(b)), int64(16384), s.connWindow, s.streamWindow))
		if n > 0 {
			s.connWindow -= int64(n)
			s.streamWindow -= int64(n)
		}
		wake := s.wake
		s.mu.Unlock()
		if n <= 0 {
			select {
			case <-s.ctx.Done():
				return net.ErrClosed
			case <-ctx.Done():
				s.close()
				return ctx.Err()
			case <-wake:
				continue
			}
		}
		if err := s.write(func() error { return s.fr.WriteData(1, false, b[:n]) }); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
