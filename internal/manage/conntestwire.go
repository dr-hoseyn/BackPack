package manage

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

const ctWireMagic = "BPCT2"
const ctWireOverhead = len(ctWireMagic) + chacha20poly1305.NonceSize + chacha20poly1305.Overhead

// Every request has a fresh nonce. Replies authenticate that request's nonce
// too, so a response from another poll cannot be substituted. The link secret
// and permanent setup credentials stay inside the encrypted envelope.
func ctSeal(secret, kind string, requestNonce, payload []byte) ([]byte, []byte, error) {
	key := sha256.Sum256([]byte("backpack-conntest-control-v2|" + secret))
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	out := append([]byte(ctWireMagic), nonce...)
	aad := append([]byte("backpack-ct2/"+kind+"/"), requestNonce...)
	return aead.Seal(out, nonce, payload, aad), nonce, nil
}
func ctOpen(secret, kind string, requestNonce, packet []byte) ([]byte, []byte, error) {
	if len(packet) < ctWireOverhead || !bytes.Equal(packet[:len(ctWireMagic)], []byte(ctWireMagic)) {
		return nil, nil, fmt.Errorf("encrypted test packet is missing or truncated; update both peers")
	}
	key := sha256.Sum256([]byte("backpack-conntest-control-v2|" + secret))
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return nil, nil, err
	}
	nonce := packet[len(ctWireMagic) : len(ctWireMagic)+aead.NonceSize()]
	aad := append([]byte("backpack-ct2/"+kind+"/"), requestNonce...)
	plain, err := aead.Open(nil, nonce, packet[len(ctWireMagic)+aead.NonceSize():], aad)
	if err != nil {
		return nil, nil, fmt.Errorf("test packet authentication failed")
	}
	return plain, nonce, nil
}
func ctReadPacket(r io.Reader) ([]byte, error) { return ctReadPacketLimit(r, 256<<10) }
func ctReadPacketLimit(r io.Reader, limit uint32) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n < uint32(ctWireOverhead) || n > limit {
		return nil, fmt.Errorf("invalid test packet length")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
func ctWritePacket(w io.Writer, b []byte) error {
	if len(b) > 256<<10 {
		return fmt.Errorf("test packet is too large")
	}
	// One write avoids a tiny header packet delaying the body behind Nagle and
	// delayed ACKs on every short-lived coordinator connection.
	packet := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(packet[:4], uint32(len(b)))
	copy(packet[4:], b)
	_, err := io.Copy(w, bytes.NewReader(packet))
	return err
}
func (c *ctCoordinator) wireAnswer(packet []byte, from string) []byte {
	plain, nonce, err := ctOpen(c.tok, "request", nil, packet)
	if err != nil {
		return nil
	}
	reply := c.answer(string(plain), from)
	if reply == "" {
		return nil
	}
	wire, _, err := ctSeal(c.tok, "reply", nonce, []byte(reply))
	if err != nil {
		return nil
	}
	return wire
}
