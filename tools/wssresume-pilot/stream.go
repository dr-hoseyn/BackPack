package main

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	chunkSize          = 32 << 10
	window             = 64
	resumeTimeout      = 7 * time.Second
	dataFrame     byte = iota + 1
	ackFrame
	finFrame
)
const eofBit = 1 << 31

var (
	errProtocol = errors.New("invalid resume frame")
	errLocal    = errors.New("local endpoint closed")
	blockBudget = make(chan struct{}, (128<<20)/chunkSize)
	blockPool   = sync.Pool{New: func() any { return new([chunkSize]byte) }}
)

type chunk struct {
	at   uint64
	buf  *[chunkSize]byte
	n    int
	refs atomic.Int32
}

func (b *chunk) release() {
	if b.refs.Add(-1) == 0 {
		blockPool.Put(b.buf)
		<-blockBudget
	}
}

type stream struct {
	app   *net.TCPConn
	done  chan struct{}
	wake  chan struct{}
	space chan struct{}
	once  sync.Once

	mu                             sync.Mutex
	pending                        []*chunk
	out, ack, received             uint64
	localEOF, peerEOF, receivedEOF bool

	// Only the server uses attach to fence a stale carrier before talking
	// to a replacement. Fence the old receiver before reading byte offsets.
	attach sync.Mutex
	active *carrier
}

type carrier struct {
	conn *net.TCPConn
	done chan struct{}
}

func newStream(c *net.TCPConn) *stream {
	return &stream{app: c, done: make(chan struct{}), wake: make(chan struct{}, 1), space: make(chan struct{}, 1)}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *stream) close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.app.Close()
		s.mu.Lock()
		for _, b := range s.pending {
			b.release()
		}
		s.pending = nil
		s.mu.Unlock()
	})
}

func (s *stream) collect() {
	for {
		s.mu.Lock()
		full := len(s.pending) >= window
		s.mu.Unlock()
		if full {
			select {
			case <-s.done:
				return
			case <-s.space:
			}
			continue
		}
		select {
		case <-s.done:
			return
		case blockBudget <- struct{}{}:
		}
		buf := blockPool.Get().(*[chunkSize]byte)
		n, err := s.app.Read(buf[:])
		s.mu.Lock()
		select {
		case <-s.done:
			s.mu.Unlock()
			blockPool.Put(buf)
			<-blockBudget
			return
		default:
		}
		if n != 0 {
			b := &chunk{at: s.out, buf: buf, n: n}
			b.refs.Store(1)
			s.pending = append(s.pending, b)
			s.out += uint64(n)
		} else {
			blockPool.Put(buf)
			<-blockBudget
		}
		if err == io.EOF {
			s.localEOF = true
		}
		s.mu.Unlock()
		signal(s.wake)
		if err != nil {
			if err != io.EOF {
				s.close()
			}
			return
		}
	}
}

func (s *stream) acknowledge(offset uint64, eof bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if offset < s.ack || offset > s.out || (eof && (!s.localEOF || offset != s.out)) || (s.peerEOF && !eof) {
		return errProtocol
	}
	s.ack = offset
	s.peerEOF = eof
	for len(s.pending) > 0 {
		b := s.pending[0]
		if b.at+uint64(b.n) > offset {
			break
		}
		s.pending[0] = nil
		s.pending = s.pending[1:]
		b.release()
	}
	signal(s.space)
	signal(s.wake)
	return nil
}

func (s *stream) receiveState() (uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received, s.receivedEOF
}

func writePacket(c *net.TCPConn, kind byte, offset uint64, raw uint32, data []byte) error {
	var header [13]byte
	header[0] = kind
	binary.BigEndian.PutUint64(header[1:], offset)
	binary.BigEndian.PutUint32(header[9:], raw)
	// Bound a stalled replacement attempt without imposing a transfer rate.
	_ = c.SetWriteDeadline(time.Now().Add(resumeTimeout))
	buffers := net.Buffers{header[:], data}
	_, err := buffers.WriteTo(c)
	return err
}

func (s *stream) send(c *net.TCPConn, offset uint64, initialEOF bool, lastAck uint64, sentAckEOF bool, retired <-chan struct{}) error {
	sentFin := initialEOF
	for {
		select {
		case <-s.done:
			return errLocal
		case <-retired:
			return net.ErrClosed
		default:
		}
		s.mu.Lock()
		received, receivedEOF, peerEOF := s.received, s.receivedEOF, s.peerEOF
		if lastAck != received || (receivedEOF && !sentAckEOF) {
			s.mu.Unlock()
			var flags uint32
			if receivedEOF {
				flags = eofBit
			}
			if err := writePacket(c, ackFrame, received, flags, nil); err != nil {
				return err
			}
			lastAck, sentAckEOF = received, receivedEOF
			continue
		}
		if receivedEOF && sentAckEOF && peerEOF {
			s.mu.Unlock()
			s.close()
			return nil
		}
		var block *chunk
		if offset < s.out {
			for _, b := range s.pending {
				if offset >= b.at && offset < b.at+uint64(b.n) {
					block = b
					block.refs.Add(1)
					break
				}
			}
			if block == nil {
				s.mu.Unlock()
				return errProtocol
			}
		}
		fin := s.localEOF && offset == s.out && !sentFin
		s.mu.Unlock()
		if block != nil {
			payload := block.buf[int(offset-block.at):block.n]
			err := writePacket(c, dataFrame, offset, uint32(len(payload)), payload)
			block.release()
			if err != nil {
				return err
			}
			offset += uint64(len(payload))
			continue
		}
		if fin {
			if err := writePacket(c, finFrame, offset, 0, nil); err != nil {
				return err
			}
			sentFin = true
			continue
		}
		select {
		case <-s.done:
			return errLocal
		case <-retired:
			return net.ErrClosed
		case <-s.wake:
		}
	}
}

func (s *stream) receive(c *net.TCPConn) error {
	var header [13]byte
	buf := make([]byte, chunkSize)
	for {
		if _, err := io.ReadFull(c, header[:]); err != nil {
			return err
		}
		offset := binary.BigEndian.Uint64(header[1:])
		raw := binary.BigEndian.Uint32(header[9:])
		switch header[0] {
		case ackFrame:
			if raw != 0 && raw != eofBit {
				return errProtocol
			}
			if err := s.acknowledge(offset, raw == eofBit); err != nil {
				return err
			}
		case dataFrame:
			if raw == 0 || raw > chunkSize || offset+uint64(raw) < offset {
				return errProtocol
			}
			if _, err := io.ReadFull(c, buf[:raw]); err != nil {
				return err
			}
			next, eof := s.receiveState()
			if offset > next || eof {
				return errProtocol
			}
			end := offset + uint64(raw)
			if end <= next {
				continue
			}
			_ = s.app.SetWriteDeadline(time.Now().Add(resumeTimeout))
			if _, err := s.app.Write(buf[next-offset : raw]); err != nil {
				s.close()
				return errLocal
			}
			s.mu.Lock()
			s.received = end
			s.mu.Unlock()
			signal(s.wake)
		case finFrame:
			next, eof := s.receiveState()
			if raw != 0 || offset != next {
				return errProtocol
			}
			if eof {
				continue
			}
			if err := s.app.CloseWrite(); err != nil {
				s.close()
				return errLocal
			}
			s.mu.Lock()
			s.receivedEOF = true
			s.mu.Unlock()
			signal(s.wake)
		default:
			return errProtocol
		}
	}
}

func (s *stream) transfer(link *carrier, offset uint64, eof bool, localOffset uint64, localEOF bool) {
	defer close(link.done)
	retired := make(chan struct{})
	results := make(chan error, 2)
	go func() { results <- s.send(link.conn, offset, eof, localOffset, localEOF, retired) }()
	go func() { results <- s.receive(link.conn) }()
	var first error
	receivedFirst := false
	select {
	case first = <-results:
		receivedFirst = true
	case <-s.done:
	}
	close(retired)
	_ = link.conn.Close()
	err := <-results
	if !receivedFirst {
		first = <-results
	}
	if first == errProtocol || err == errProtocol {
		s.close()
	}
}
