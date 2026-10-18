// Package pathprobe verifies a managed path without claiming its reverse
// control channel or opening a backend. Both directions carry bounded known
// payloads; a TLS handshake or a ready local helper cannot pass this test.
package pathprobe

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"time"
)

func mac(token, label string, b []byte) []byte {
	k := sha256.Sum256([]byte("backpack managed path probe v1\x00" + token))
	h := hmac.New(sha256.New, k[:])
	h.Write([]byte(label))
	h.Write(b)
	return h.Sum(nil)
}

func payload(n int, nonce []byte, label string) []byte {
	seed := sha256.Sum256(append(append([]byte(nil), nonce...), []byte(label)...))
	b := make([]byte, n)
	for i := range b {
		b[i] = seed[i%len(seed)]
	}
	return b
}

type Result struct {
	// Authenticated is true only after the peer's token-bound proof verifies.
	// It remains available when the subsequent payload transfer fails.
	Authenticated  bool
	RTT            time.Duration
	BytesPerSecond float64
}

func Request(token string, size int) (string, []byte, error) {
	if size < 1024 || size > 65536 {
		return "", nil, errors.New("probe payload must be 1024..65536 bytes")
	}
	b := make([]byte, 36)
	if _, err := rand.Read(b[:32]); err != nil {
		return "", nil, err
	}
	binary.BigEndian.PutUint32(b[32:], uint32(size))
	return hex.EncodeToString(append(b, mac(token, "request", b)...)), b, nil
}

// Check runs after the request announcement has crossed the managed carrier.
func Check(conn net.Conn, token string, request []byte, started time.Time) (Result, error) {
	var result Result
	proof := make([]byte, 32)
	if _, err := io.ReadFull(conn, proof); err != nil {
		return result, err
	}
	if !hmac.Equal(proof, mac(token, "server", request)) {
		return result, errors.New("path probe server proof refused")
	}
	result.Authenticated = true
	result.RTT = time.Since(started)
	if result.RTT < time.Microsecond {
		result.RTT = time.Microsecond
	}
	size := int(binary.BigEndian.Uint32(request[32:]))
	upload := payload(size, request, "upload")
	transfer := time.Now()
	if _, err := io.Copy(conn, bytes.NewReader(upload)); err != nil {
		return result, err
	}
	checksum := make([]byte, 32)
	if _, err := io.ReadFull(conn, checksum); err != nil {
		return result, err
	}
	want := sha256.Sum256(upload)
	if !bytes.Equal(checksum, want[:]) {
		return result, errors.New("path probe upload corrupt")
	}
	download := make([]byte, size)
	if _, err := io.ReadFull(conn, download); err != nil {
		return result, err
	}
	if !bytes.Equal(download, payload(size, request, "download")) {
		return result, errors.New("path probe download corrupt")
	}
	elapsed := max(time.Since(transfer), time.Microsecond)
	result.BytesPerSecond = float64(size*2) / elapsed.Seconds()
	return result, nil
}

func Serve(conn net.Conn, token, announcement string) error {
	defer conn.Close()
	if len(announcement) != 136 {
		return errors.New("invalid path probe request")
	}
	b, err := hex.DecodeString(announcement)
	if err != nil {
		return err
	}
	request, proof := b[:36], b[36:]
	if !hmac.Equal(proof, mac(token, "request", request)) {
		return errors.New("path probe authentication refused")
	}
	size := int(binary.BigEndian.Uint32(request[32:]))
	if size < 1024 || size > 65536 {
		return errors.New("probe size exceeds bounds")
	}
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write(mac(token, "server", request)); err != nil {
		return err
	}
	upload := make([]byte, size)
	if _, err := io.ReadFull(conn, upload); err != nil {
		return err
	}
	if !bytes.Equal(upload, payload(size, request, "upload")) {
		return errors.New("path probe upload corrupt")
	}
	checksum := sha256.Sum256(upload)
	if _, err := conn.Write(checksum[:]); err != nil {
		return err
	}
	_, err = io.Copy(conn, bytes.NewReader(payload(size, request, "download")))
	return err
}
