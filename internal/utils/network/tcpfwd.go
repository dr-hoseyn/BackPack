package network

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/flynn/noise"
)

const (
	tcpfwdHeader      = "PGprotocol-NUM3"
	tcpfwdControl     = 'C'
	tcpfwdData        = 'D'
	TCPFWDIdleTimeout = 25 * time.Second
)

var tcpfwdSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256)

func tcpfwdNoise(kind byte) noiseProfile {
	return noiseProfile{tcpfwdSuite, "backpack-tcpfwd-v1/" + string(kind), "backpack-tcpfwd-psk-v1"}
}

// TCPFWDConn protects control messages and data-connection setup.
type TCPFWDConn struct {
	net.Conn
	raw net.Conn
}

// DataConn returns the plaintext stream after both setup messages have been exchanged.
func (c *TCPFWDConn) DataConn() net.Conn { return c.raw }

// TCPFWDClientConn prefixes the connection and authenticates it with Noise
// using AES-256-GCM.
func TCPFWDClientConn(raw net.Conn, token string, control bool, timeout time.Duration) (*TCPFWDConn, error) {
	kind := byte(tcpfwdData)
	if control {
		kind = tcpfwdControl
	}
	preface := tcpfwdHeader + string(kind)
	if err := tcpfwdPrefaceDeadline(raw, timeout); err != nil {
		return nil, err
	}
	n, err := io.WriteString(raw, preface)
	if err != nil {
		return nil, err
	}
	if n != len(preface) {
		return nil, io.ErrShortWrite
	}
	secure, err := noiseHandshake(raw, token, true, timeout, tcpfwdNoise(kind))
	if err != nil {
		return nil, err
	}
	return &TCPFWDConn{Conn: secure, raw: raw}, nil
}

// TCPFWDServerConn validates the prefix before running the matching handshake.
func TCPFWDServerConn(raw net.Conn, token string, timeout time.Duration) (*TCPFWDConn, bool, error) {
	if err := tcpfwdPrefaceDeadline(raw, timeout); err != nil {
		return nil, false, err
	}
	var preface [len(tcpfwdHeader) + 1]byte
	if _, err := io.ReadFull(raw, preface[:]); err != nil {
		return nil, false, err
	}
	kind := preface[len(tcpfwdHeader)]
	if string(preface[:len(tcpfwdHeader)]) != tcpfwdHeader || (kind != tcpfwdControl && kind != tcpfwdData) {
		return nil, false, errors.New("invalid tcpfwd preface")
	}
	secure, err := noiseHandshake(raw, token, false, timeout, tcpfwdNoise(kind))
	if err != nil {
		return nil, false, err
	}
	return &TCPFWDConn{Conn: secure, raw: raw}, kind == tcpfwdControl, nil
}

func tcpfwdPrefaceDeadline(raw net.Conn, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = noiseHandshakeTimeout
	}
	return raw.SetDeadline(time.Now().Add(timeout))
}
