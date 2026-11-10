package network

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"time"
)

// The two openings and bootstrap record shapes were measured against NUM3.
const (
	tcpfwdXMPPClient  = "<?xml version='1.0'?><stream:stream to='example.com' xmlns='jabber:client' version='1.0'>\r\n"
	tcpfwdXMPPServer  = "<stream:stream xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams' version='1.0'>\r\n"
	num3RawFlag       = uint32(1 << 31)
	num3MaxRecord     = (1<<16 - 1) + 11 + 28 // destination, open fields, nonce and tag
	num3AdaptiveOffer = "BPPOOL01"

	// TCPFWDNUM3KeepAlive is the interval observed between idle pool frames.
	TCPFWDNUM3KeepAlive = time.Second
)

var (
	num3ClientHello = [34]byte{'P', 'E', 'N', 'G', 'U', '-', 'L', '4', 7}
	num3ServerHello = [18]byte{'P', 'E', 'N', 'G', 'U', '-', 'L', '4', 7, 1}
	num3Control     = []byte{6, 0, 0, 0, 0}
	num3Markers     = [][]byte{
		{6, 0, 0, 0, 0, 'P', 'C', 'T', 'L'},
		{6, 0, 0, 0, 0, 'P', 'B', 'A', 'N'},
	}
)

// TCPFWDXMPPConn protects Backpack's control stream inside NUM3 records. Pool
// connections switch to the NUM3 stream adapter after their admission message.
type TCPFWDXMPPConn struct {
	net.Conn
	// AdaptivePool is confirmed by both authenticated bootstrap hellos.
	AdaptivePool bool
	codec        *num3Records
	pending      []byte
	edge         bool
	readMu       sync.Mutex
	writeMu      sync.Mutex
}

type num3Records struct {
	conn              io.ReadWriter
	aead              cipher.AEAD
	dataAEAD          cipher.AEAD
	readAAD, writeAAD *[9]byte
	readHeader        [4]byte
	readDataAAD       [14]byte
	writeDataAAD      [14]byte
	writeBuffer       []byte
}

func num3AEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (r *num3Records) encode(body []byte) ([]byte, error) {
	length := len(body) + r.aead.Overhead()
	if len(body) == 0 || length > num3MaxRecord {
		return nil, errors.New("invalid NUM3 record length")
	}
	frame := make([]byte, 4, 4+length)
	binary.BigEndian.PutUint32(frame, uint32(length))
	return r.aead.Seal(frame, nil, body, nextNUM3AAD(r.writeAAD)), nil
}

func (r *num3Records) write(body []byte) error {
	frame, err := r.encode(body)
	if err != nil {
		return err
	}
	return writeNUM3Bytes(r.conn, frame)
}

// Data exposes its type and stream ID in the raw envelope. The payload has
// its own key and carries a nonce and tag even when the envelope is raw.
func (r *num3Records) writeData(id uint32, payload []byte) error {
	size := num3StreamHeaderSize + len(payload) + r.dataAEAD.Overhead()
	frame := slices.Grow(r.writeBuffer[:0], 4+size)[:4]
	binary.BigEndian.PutUint32(frame, num3RawFlag|uint32(size))
	frame = appendNUM3StreamFrame(frame, num3Data, id, nil)
	aad := num3DataAAD(&r.writeDataAAD, r.writeAAD, frame[4:])
	frame = r.dataAEAD.Seal(frame, nil, payload, aad)
	r.writeBuffer = frame
	return writeNUM3Bytes(r.conn, frame)
}

func (r *num3Records) openData(header, ciphertext []byte) ([]byte, error) {
	aad := num3DataAAD(&r.readDataAAD, r.readAAD, header)
	return r.dataAEAD.Open(ciphertext[:0], nil, ciphertext, aad)
}

func num3DataAAD(aad *[14]byte, sequence *[9]byte, header []byte) []byte {
	copy(aad[:9], nextNUM3AAD(sequence))
	copy(aad[9:], header)
	return aad[:]
}

func (r *num3Records) read() ([]byte, bool, error) {
	return r.readInto(nil)
}

// readInto appends a record to storage, preserving any header space the
// caller left at its beginning.
func (r *num3Records) readInto(storage []byte) (record []byte, raw bool, err error) {
	if _, err := io.ReadFull(r.conn, r.readHeader[:]); err != nil {
		return nil, false, err
	}
	word := binary.BigEndian.Uint32(r.readHeader[:])
	raw = word&num3RawFlag != 0
	length := int(word &^ num3RawFlag)
	if length <= 0 || length > num3MaxRecord || (!raw && length <= r.aead.Overhead()) {
		return nil, false, errors.New("invalid NUM3 record length")
	}
	prefix := len(storage)
	record = slices.Grow(storage, length)[:prefix+length]
	body := record[prefix:]
	if _, err := io.ReadFull(r.conn, body); err != nil {
		return nil, false, err
	}
	if !raw {
		body, err = r.aead.Open(body[:0], nil, body, nextNUM3AAD(r.readAAD))
	}
	return record[:prefix+len(body)], raw, err
}

// Sequence protected records independently by direction, so a captured control
// message or stream open cannot be reflected or replayed within the session.
func nextNUM3AAD(sequence *[9]byte) []byte {
	if sequence == nil {
		return nil // Reference bootstrap format.
	}
	binary.BigEndian.PutUint64(sequence[1:], binary.BigEndian.Uint64(sequence[1:])+1)
	return sequence[:]
}

func writeNUM3Bytes(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

func readNUM3Opening(conn net.Conn, expected string) error {
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(conn, actual); err != nil {
		return err
	}
	if string(actual) != expected {
		return errors.New("invalid NUM3 XML opening")
	}
	return nil
}

// TCPFWDXMPPClientConn reads the Iran opening before sending the exit opening.
func TCPFWDXMPPClientConn(raw net.Conn, token string, timeout time.Duration) (*TCPFWDXMPPConn, error) {
	if err := tcpfwdPrefaceDeadline(raw, timeout); err != nil {
		return nil, err
	}
	if err := readNUM3Opening(raw, tcpfwdXMPPServer); err != nil {
		return nil, err
	}
	if err := writeNUM3Bytes(raw, []byte(tcpfwdXMPPClient)); err != nil {
		return nil, err
	}
	return tcpfwdNUM3Handshake(raw, token, false)
}

// TCPFWDXMPPServerConn sends the Iran opening before waiting for the exit.
func TCPFWDXMPPServerConn(raw net.Conn, token string, timeout time.Duration) (*TCPFWDXMPPConn, error) {
	if err := tcpfwdPrefaceDeadline(raw, timeout); err != nil {
		return nil, err
	}
	if err := writeNUM3Bytes(raw, []byte(tcpfwdXMPPServer)); err != nil {
		return nil, err
	}
	if err := readNUM3Opening(raw, tcpfwdXMPPClient); err != nil {
		return nil, err
	}
	return tcpfwdNUM3Handshake(raw, token, true)
}

func tcpfwdNUM3Handshake(raw net.Conn, token string, edge bool) (*TCPFWDXMPPConn, error) {
	key, err := hkdf.Key(sha256.New, []byte(token), []byte("pengutunnel-salt-v1"), "tunnel-aead-key", 32)
	if err != nil {
		return nil, err
	}
	aead, err := num3AEAD(key)
	if err != nil {
		return nil, err
	}
	bootstrap := num3Records{conn: raw, aead: aead}
	var salt [28]byte
	adaptive := false
	if edge {
		hello, plain, err := bootstrap.read()
		if err != nil {
			return nil, err
		}
		if plain || len(hello) != len(num3ClientHello) ||
			!bytes.Equal(hello[:10], num3ClientHello[:10]) || !bytes.Equal(hello[26:], num3ClientHello[26:]) {
			return nil, errors.New("invalid NUM3 exit hello")
		}
		copy(salt[:16], hello[10:26])
		adaptive = string(hello[10:18]) == num3AdaptiveOffer
		answer := num3ServerHello
		if adaptive {
			answer[9] = 2
		}
		reply, err := bootstrap.encode(answer[:])
		if err != nil {
			return nil, err
		}
		copy(salt[16:], reply[4:16])
		if err := writeNUM3Bytes(raw, reply); err != nil {
			return nil, err
		}
	} else {
		hello := num3ClientHello
		if _, err := rand.Read(hello[10:26]); err != nil {
			return nil, err
		}
		// Keep 64 random bits in the offer, alongside the responder's
		// independent 96-bit nonce used by the session key.
		copy(hello[10:18], num3AdaptiveOffer)
		copy(salt[:16], hello[10:26])
		if err := bootstrap.write(hello[:]); err != nil {
			return nil, err
		}
		// Preserve the responder's random nonce for session key derivation.
		var reply [4 + 12 + len(num3ServerHello) + 16]byte
		if _, err := io.ReadFull(raw, reply[:4]); err != nil {
			return nil, err
		}
		if binary.BigEndian.Uint32(reply[:4]) != uint32(len(reply)-4) {
			return nil, errors.New("invalid NUM3 edge hello length")
		}
		if _, err := io.ReadFull(raw, reply[4:]); err != nil {
			return nil, err
		}
		answer, err := aead.Open(nil, nil, reply[4:], nil)
		if err == nil && len(answer) == len(num3ServerHello) && answer[9] == 2 {
			adaptive = true
			answer[9] = num3ServerHello[9]
		}
		if err != nil || !bytes.Equal(answer, num3ServerHello[:]) {
			return nil, errors.New("invalid NUM3 edge hello")
		}
		copy(salt[16:], reply[4:16])
	}
	// Bind both sides to this connection. A recorded peer exchange cannot
	// seat a client or admit streams on a fresh connection.
	sessionKey, err := hkdf.Key(sha256.New, key, salt[:], "backpack-num3-session-v1", 32)
	if err != nil {
		return nil, err
	}
	sessionAEAD, err := num3AEAD(sessionKey)
	if err != nil {
		return nil, err
	}
	dataKey, err := hkdf.Key(sha256.New, sessionKey, nil, "backpack-num3-data-v1", 32)
	if err != nil {
		return nil, err
	}
	dataAEAD, err := num3AEAD(dataKey)
	if err != nil {
		return nil, err
	}
	codec := &num3Records{conn: raw, aead: sessionAEAD, dataAEAD: dataAEAD, readAAD: &[9]byte{1}, writeAAD: &[9]byte{2}}
	if edge {
		codec.readAAD, codec.writeAAD = codec.writeAAD, codec.readAAD
	}
	if err := codec.exchangeMarkers(edge); err != nil {
		return nil, err
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return &TCPFWDXMPPConn{Conn: raw, codec: codec, edge: edge, AdaptivePool: adaptive}, nil
}

// EncodeNUM3Pool includes the configured pool floor only when the bootstrap
// confirmed support. The nonce retains its full original authentication.
func EncodeNUM3Pool(nonce string, floor int) (string, error) {
	if len(nonce) != poolNonceHexLen || floor < 1 || floor > 256 {
		return "", errors.New("invalid NUM3 pool announcement")
	}
	return nonce + string([]byte{byte(floor >> 8), byte(floor)}), nil
}

func DecodeNUM3Pool(payload string) (nonce string, floor int, err error) {
	if len(payload) != poolNonceHexLen+2 {
		return "", 0, errors.New("invalid NUM3 pool announcement")
	}
	floor = int(binary.BigEndian.Uint16([]byte(payload[poolNonceHexLen:])))
	if floor < 1 || floor > 256 {
		return "", 0, errors.New("invalid NUM3 pool floor")
	}
	return payload[:poolNonceHexLen], floor, nil
}

func (r *num3Records) exchangeMarkers(edge bool) error {
	send := func() error {
		var batch []byte
		for _, marker := range num3Markers {
			frame, err := r.encode(marker)
			if err != nil {
				return err
			}
			batch = append(batch, frame...)
		}
		return writeNUM3Bytes(r.conn, batch)
	}
	read := func() error {
		for _, marker := range num3Markers {
			value, raw, err := r.read()
			if err != nil {
				return err
			}
			if raw || !bytes.Equal(value, marker) {
				return errors.New("invalid NUM3 control confirmation")
			}
		}
		return nil
	}
	first, second := read, send
	if edge {
		first, second = send, read
	}
	if err := first(); err != nil {
		return err
	}
	return second()
}

func (c *TCPFWDXMPPConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(c.pending) == 0 {
		body, raw, err := c.codec.read()
		if err != nil {
			return 0, err
		}
		if raw || len(body) <= len(num3Control) || !bytes.HasPrefix(body, num3Control) {
			return 0, errors.New("invalid NUM3 control frame")
		}
		c.pending = body[len(num3Control):]
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *TCPFWDXMPPConn) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	written := 0
	for len(p) > 0 {
		size := min(len(p), num3MaxRecord-len(num3Control)-c.codec.aead.Overhead())
		body := append(bytes.Clone(num3Control), p[:size]...)
		if err := c.codec.write(body); err != nil {
			_ = c.Close()
			return written, err
		}
		written += size
		p = p[size:]
	}
	return written, nil
}

func (c *TCPFWDXMPPConn) DataConn() net.Conn {
	limitTCPMuxBacklog(c.Conn)
	return &num3MuxConn{Conn: c.Conn, codec: c.codec, edge: c.edge, opening: make(map[uint32][]byte)}
}
