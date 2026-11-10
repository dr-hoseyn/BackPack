package network

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"
)

// These are the complete first payloads seen in both directions of the
// PenguTunnel NUM3 configuration. The authenticated Backpack exchange follows.
const (
	tcpfwdXMPPClient = "<?xml version='1.0'?><stream:stream to='example.com' xmlns='jabber:client' version='1.0'>\r\n"
	tcpfwdXMPPServer = "<stream:stream xmlns='jabber:client' xmlns:stream='http://etherx.jabber.org/streams' version='1.0'>\r\n"
)

var tcpfwdXMPPNoise = noiseProfile{tcpfwdSuite, "backpack-tcpfwd-xmpp-v1", "backpack-tcpfwd-xmpp-psk-v1"}

// TCPFWDXMPPClientConn exchanges the XMPP-shaped prefaces and authenticates the
// connection with the separate XMPP profile.
func TCPFWDXMPPClientConn(raw net.Conn, token string, timeout time.Duration) (*TCPFWDConn, error) {
	if err := tcpfwdPrefaceDeadline(raw, timeout); err != nil {
		return nil, err
	}
	if err := writeTCPFWDOpening(raw, tcpfwdXMPPClient); err != nil {
		return nil, err
	}
	if err := readTCPFWDOpening(raw, tcpfwdXMPPServer); err != nil {
		return nil, err
	}
	return tcpfwdXMPPHandshake(raw, token, true, timeout)
}

// TCPFWDXMPPServerConn consumes the client opener before sending its response.
func TCPFWDXMPPServerConn(raw net.Conn, token string, timeout time.Duration) (*TCPFWDConn, error) {
	if err := tcpfwdPrefaceDeadline(raw, timeout); err != nil {
		return nil, err
	}
	if err := readTCPFWDOpening(raw, tcpfwdXMPPClient); err != nil {
		return nil, err
	}
	if err := writeTCPFWDOpening(raw, tcpfwdXMPPServer); err != nil {
		return nil, err
	}
	return tcpfwdXMPPHandshake(raw, token, false, timeout)
}

func readTCPFWDOpening(conn net.Conn, expected string) error {
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(conn, actual); err != nil {
		return err
	}
	if string(actual) != expected {
		return errors.New("invalid tcpfwd XMPP opening")
	}
	return nil
}

func writeTCPFWDOpening(conn net.Conn, opening string) error {
	n, err := io.WriteString(conn, opening)
	if err != nil {
		return err
	}
	if n != len(opening) {
		return io.ErrShortWrite
	}
	return nil
}

func tcpfwdXMPPHandshake(raw net.Conn, token string, initiator bool, timeout time.Duration) (*TCPFWDConn, error) {
	secure, err := noiseHandshake(raw, token, initiator, timeout, tcpfwdXMPPNoise)
	if err != nil {
		return nil, err
	}
	return &TCPFWDConn{Conn: secure, raw: raw}, nil
}

// TCPFWDTargets protects destination metadata on an otherwise plaintext mux
// session. Each session uses a key derived from its Noise handshake, and the
// associated data binds a destination to its stream ID.
type TCPFWDTargets struct {
	aead cipher.AEAD
}

// TargetWriter takes the responder's outgoing traffic key. Only the responder
// opens mux streams and writes destinations.
func (c *TCPFWDConn) TargetWriter() (*TCPFWDTargets, error) {
	return newTCPFWDTargets(c.Conn.(*noiseConn).send.UnsafeKey())
}

// TargetReader uses the corresponding incoming key on the initiator.
func (c *TCPFWDConn) TargetReader() (*TCPFWDTargets, error) {
	return newTCPFWDTargets(c.Conn.(*noiseConn).recv.UnsafeKey())
}

func newTCPFWDTargets(secret [32]byte) (*TCPFWDTargets, error) {
	key, err := hkdf.Key(sha256.New, secret[:], nil, "backpack-tcpfwd-xmpp-targets-v1", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &TCPFWDTargets{aead}, nil
}

func (t *TCPFWDTargets) Seal(streamID uint32, target string) string {
	var stream [4]byte
	binary.BigEndian.PutUint32(stream[:], streamID)
	return string(t.aead.Seal(nil, nil, []byte(target), stream[:]))
}

func (t *TCPFWDTargets) Open(streamID uint32, record string) (string, error) {
	var stream [4]byte
	binary.BigEndian.PutUint32(stream[:], streamID)
	plain, err := t.aead.Open(nil, nil, []byte(record), stream[:])
	return string(plain), err
}
