package network

import (
	"encoding/binary"
	"errors"
	"math"
	"net"
)

const (
	num3Open             byte = 1
	num3Data             byte = 2
	num3FIN              byte = 3
	num3StreamHeaderSize      = 5
	smuxHeaderSize            = 8

	smuxSYN byte = 0
	smuxFIN byte = 1
	smuxPSH byte = 2
	smuxNOP byte = 3
)

// num3MuxConn keeps smux's session management and worker lifecycle internal.
// On the TCP connection the only stream framing is NUM3. In particular, the
// encrypted open carries the destination. The raw data envelope exposes the
// type and stream ID and encloses an encrypted payload.
type num3MuxConn struct {
	net.Conn
	codec      *num3Records
	edge       bool
	opening    map[uint32][]byte // only touched by the session's single writer
	pending    []byte            // only touched by the session's single reader
	readBuffer []byte
	queueTune  func() // the smux writer calls this only on an existing heartbeat
}

func appendSMUXFrame(dst []byte, kind byte, id uint32, body []byte) []byte {
	var header [smuxHeaderSize]byte
	header[0], header[1] = 1, kind
	binary.LittleEndian.PutUint16(header[2:4], uint16(len(body)))
	binary.LittleEndian.PutUint32(header[4:8], id)
	dst = append(dst, header[:]...)
	return append(dst, body...)
}

func appendNUM3StreamFrame(dst []byte, kind byte, id uint32, body []byte) []byte {
	var header [num3StreamHeaderSize]byte
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:], id)
	dst = append(dst, header[:]...)
	return append(dst, body...)
}

func (c *num3MuxConn) Write(p []byte) (int, error) {
	if len(p) < smuxHeaderSize {
		return 0, errors.New("invalid internal NUM3 mux frame")
	}
	return c.writeSMUX(p[:smuxHeaderSize], p[smuxHeaderSize:])
}

// The smux sender supplies its header and payload separately. Encrypt the
// payload directly from that slice instead of packing an intermediate frame.
func (c *num3MuxConn) WriteBuffers(parts [][]byte) (int, error) {
	if len(parts) != 2 {
		return 0, errors.New("invalid internal NUM3 mux write")
	}
	return c.writeSMUX(parts[0], parts[1])
}

func (c *num3MuxConn) writeSMUX(header, body []byte) (int, error) {
	if len(header) != smuxHeaderSize || header[0] != 1 || int(binary.LittleEndian.Uint16(header[2:4])) != len(body) {
		return 0, errors.New("invalid internal NUM3 mux frame")
	}
	written := len(header) + len(body)
	kind := header[1]
	if kind == smuxNOP {
		if c.queueTune != nil {
			c.queueTune()
		}
		if err := c.codec.write(num3Control); err != nil {
			return 0, err
		}
		return written, nil
	}
	id := binary.LittleEndian.Uint32(header[4:8])
	if id < 3 || id%2 != 1 {
		return 0, errors.New("invalid NUM3 mux stream ID")
	}
	wireID := id - 2
	switch kind {
	case smuxSYN:
		if !c.edge || len(body) != 0 {
			return 0, errors.New("unexpected NUM3 stream open")
		}
		c.opening[id] = nil
	case smuxFIN:
		if _, ok := c.opening[id]; ok {
			delete(c.opening, id)
		} else if err := c.codec.write(appendNUM3StreamFrame(nil, num3FIN, wireID, nil)); err != nil {
			return 0, err
		}
	case smuxPSH:
		if prefix, ok := c.opening[id]; ok {
			var err error
			if len(prefix) > 0 {
				body = append(prefix, body...)
			}
			body, err = c.writeOpen(id, body)
			if err != nil {
				return 0, err
			}
		}
		if len(body) > 0 {
			if err := c.codec.writeData(wireID, body); err != nil {
				return 0, err
			}
		}
	default:
		return 0, errors.New("unsupported NUM3 mux command")
	}
	return written, nil
}

func (c *num3MuxConn) writeOpen(id uint32, payload []byte) ([]byte, error) {
	if len(payload) < 2 {
		c.opening[id] = append([]byte(nil), payload...)
		return nil, nil
	}
	size := int(binary.BigEndian.Uint16(payload[:2]))
	if size == 0 {
		return nil, errors.New("invalid NUM3 destination length")
	}
	if len(payload) < 2+size {
		c.opening[id] = append([]byte(nil), payload...)
		return nil, nil
	}
	// TCP mode and flags, target length and text, then an empty source.
	frame := appendNUM3StreamFrame(nil, num3Open, id-2, []byte{1, 0})
	frame = append(frame, payload[:2+size]...)
	frame = append(frame, 0, 0)
	if err := c.codec.write(frame); err != nil {
		return nil, err
	}
	delete(c.opening, id)
	return payload[2+size:], nil
}

func (c *num3MuxConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Leave enough room to write the larger smux header immediately before
	// the decrypted payload. Keep the record alive until all of it is read.
	const padding = smuxHeaderSize - num3StreamHeaderSize
	if c.readBuffer == nil {
		c.readBuffer = make([]byte, padding)
	}
	for len(c.pending) == 0 {
		record, raw, err := c.codec.readInto(c.readBuffer[:padding])
		if err != nil {
			return 0, err
		}
		c.readBuffer = record
		body := record[padding:]
		if len(body) < num3StreamHeaderSize {
			return 0, errors.New("short NUM3 stream frame")
		}
		header := body[:num3StreamHeaderSize]
		kind := header[0]
		id := binary.BigEndian.Uint32(header[1:])
		body = body[num3StreamHeaderSize:]
		if kind == num3Control[0] && id == 0 && !raw && len(body) == 0 {
			c.pending = appendSMUXFrame(nil, smuxNOP, 0, nil)
			continue
		}
		if id == 0 || id%2 != 1 || id > math.MaxUint32-2 || raw != (kind == num3Data) {
			return 0, errors.New("invalid NUM3 stream frame")
		}
		switch kind {
		case num3Open:
			if c.edge {
				return 0, errors.New("unexpected NUM3 stream open")
			}
			target, err := readNUM3Target(body)
			if err != nil {
				return 0, err
			}
			c.pending = appendSMUXFrame(nil, smuxSYN, id+2, nil)
			for len(target) > 0 {
				size := min(len(target), math.MaxUint16)
				c.pending = appendSMUXFrame(c.pending, smuxPSH, id+2, target[:size])
				target = target[size:]
			}
		case num3Data:
			body, err = c.codec.openData(header, body)
			if err != nil {
				return 0, err
			}
			if len(body) == 0 || len(body) > math.MaxUint16 {
				return 0, errors.New("invalid NUM3 data length")
			}
			c.pending = appendSMUXFrame(record[:0], smuxPSH, id+2, body)
		case num3FIN:
			if len(body) != 0 {
				return 0, errors.New("invalid NUM3 close frame")
			}
			c.pending = appendSMUXFrame(nil, smuxFIN, id+2, nil)
		default:
			return 0, errors.New("unknown NUM3 stream frame")
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func readNUM3Target(body []byte) ([]byte, error) {
	if len(body) < 7 || body[0] != 1 || body[1] != 0 {
		return nil, errors.New("invalid NUM3 open header")
	}
	size := int(binary.BigEndian.Uint16(body[2:4]))
	if size == 0 || len(body) != 6+size || body[4+size] != 0 || body[5+size] != 0 {
		return nil, errors.New("invalid NUM3 destination")
	}
	return body[2 : 4+size], nil
}
