// Package masqueh2 implements a bounded, single-stream RFC 9298 HTTP/2
// CONNECT-UDP connection. HTTP/2 uses reliable RFC 9297 DATAGRAM Capsules;
// packet boundaries survive, but loss still blocks subsequent TCP delivery.
package masqueh2

import (
	"errors"
	"io"
)

const maxPacket = 65507

func appendVarint(b []byte, n uint64) []byte {
	switch {
	case n < 64:
		return append(b, byte(n))
	case n < 16384:
		return append(b, byte(n>>8)|0x40, byte(n))
	case n < 1<<30:
		return append(b, byte(n>>24)|0x80, byte(n>>16), byte(n>>8), byte(n))
	default:
		return append(b, byte(n>>56)|0xc0, byte(n>>48), byte(n>>40), byte(n>>32), byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
}

func parseVarint(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, 0
	}
	v := uint64(b[0] & 63)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	return v, n
}

func datagramCapsule(p []byte) []byte {
	b := appendVarint(nil, 0) // DATAGRAM capsule
	b = appendVarint(b, uint64(len(p)+1))
	b = append(b, 0) // context ID zero, RFC 9298 section 4
	return append(b, p...)
}

// decoder bounds a declared capsule before allocating it. Unknown capsule
// types are skipped without buffering their value, including large values.
type decoder struct {
	buf  []byte
	skip uint64
}

func (d *decoder) feed(b []byte, deliver func([]byte)) error {
	for len(b) > 0 {
		if d.skip != 0 {
			n := uint64(len(b))
			if n > d.skip {
				n = d.skip
			}
			b = b[int(n):]
			d.skip -= n
			continue
		}
		// Only a partial 16-byte header or one bounded DATAGRAM is retained.
		space := maxPacket + 32 - len(d.buf)
		if space <= 0 {
			return errors.New("oversized DATAGRAM capsule")
		}
		n := min(space, len(b))
		d.buf = append(d.buf, b[:n]...)
		b = b[n:]
		for len(d.buf) != 0 {
			typ, tn := parseVarint(d.buf)
			if tn == 0 {
				break
			}
			size, sn := parseVarint(d.buf[tn:])
			if sn == 0 {
				break
			}
			header := tn + sn
			if typ != 0 {
				available := uint64(len(d.buf) - header)
				if available < size {
					d.skip = size - available
					d.buf = d.buf[:0]
					break
				}
				d.buf = d.buf[header+int(size):]
				continue
			}
			if size == 0 || size > maxPacket+8 {
				return errors.New("invalid DATAGRAM capsule size")
			}
			if uint64(len(d.buf)-header) < size {
				break
			}
			value := d.buf[header : header+int(size)]
			id, prefix := parseVarint(value)
			if prefix == 0 {
				return errors.New("missing DATAGRAM context")
			}
			if id == 0 {
				if len(value)-prefix > maxPacket {
					return io.ErrShortBuffer
				}
				deliver(value[prefix:])
			}
			d.buf = d.buf[header+int(size):]
		}
		// Avoid retaining a large allocation through a tiny subslice.
		if len(d.buf) == 0 {
			d.buf = nil
		}
	}
	return nil
}
