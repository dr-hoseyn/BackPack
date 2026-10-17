package nativeudp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net"
	"time"
)

func signProbe(key [32]byte, b []byte) []byte {
	h := hmac.New(sha256.New, key[:])
	h.Write(b)
	return h.Sum(nil)
}

// Probe verifies native UDP through an independent association without
// installing a Noise session or replacing the live reverse peer.
func Probe(ctx context.Context, pc net.PacketConn, peer net.Addr, token string) error {
	key := sha256.Sum256([]byte("backpack native UDP path proof v1\x00" + token))
	b := make([]byte, headerSize+1024)
	b[0] = 4
	if _, err := rand.Read(b[1:]); err != nil {
		return err
	}
	b = append(b, signProbe(key, b)...)
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	pc.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := pc.WriteTo(b, peer); err != nil {
			return err
		}
		next := time.Now().Add(500 * time.Millisecond)
		if deadline.Before(next) {
			next = deadline
		}
		pc.SetReadDeadline(next)
		response := make([]byte, len(b))
		n, from, err := pc.ReadFrom(response)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() && time.Now().Before(deadline) {
				continue
			}
			return err
		}
		if from.String() != peer.String() || n != len(b) || response[0] != 5 {
			return errors.New("invalid native UDP path proof")
		}
		value, signature := response[:n-sha256.Size], response[n-sha256.Size:]
		if !bytes.Equal(value[1:], b[1:len(b)-sha256.Size]) || !hmac.Equal(signature, signProbe(key, value)) {
			return errors.New("native UDP path proof was corrupted or unauthenticated")
		}
		return nil
	}
	return errors.New("native UDP path proof timed out")
}

func (l *Link) answerProbe(b []byte, from net.Addr) {
	if len(b) != headerSize+1024+sha256.Size {
		return
	}
	value, signature := b[:len(b)-sha256.Size], b[len(b)-sha256.Size:]
	if !hmac.Equal(signature, signProbe(l.probeKey, value)) {
		return
	}
	reply := append([]byte(nil), value...)
	reply[0] = 5
	reply = append(reply, signProbe(l.probeKey, reply)...)
	_ = l.rawWrite(reply, from)
}
