package network

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

type recordingTCPFWDConn struct {
	net.Conn
	wire bytes.Buffer
}

func (c *recordingTCPFWDConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.wire.Write(p[:n])
	return n, err
}

func TestTCPFWDWireSetupAndPlainPayload(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	observed := &recordingTCPFWDConn{Conn: client}
	const token = "a-shared-test-token-for-tcpfwd"
	const setup = "pool-nonce-not-visible-on-the-wire"
	const payload = "application-bytes-visible-on-the-wire"
	done := make(chan error, 1)
	go func() {
		secure, err := TCPFWDClientConn(observed, token, false, time.Second)
		if err == nil {
			err = observed.SetDeadline(time.Now().Add(time.Second))
		}
		if err == nil {
			_, err = io.WriteString(secure, setup)
		}
		if err == nil {
			_, err = io.WriteString(secure.DataConn(), payload)
		}
		done <- err
	}()

	secure, control, err := TCPFWDServerConn(server, token, time.Second)
	if err != nil || control {
		t.Fatalf("data setup: control=%v, err=%v", control, err)
	}
	if err := server.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, len(setup))
	if _, err := io.ReadFull(secure, message); err != nil || string(message) != setup {
		t.Fatalf("secure setup = %q, %v", message, err)
	}
	body := make([]byte, len(payload))
	if _, err := io.ReadFull(secure.DataConn(), body); err != nil || string(body) != payload {
		t.Fatalf("raw payload = %q, %v", body, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	wire := observed.wire.Bytes()
	if !bytes.HasPrefix(wire, []byte("PGprotocol-NUM3D")) || !bytes.HasSuffix(wire, []byte(payload)) {
		t.Fatal("wire did not have the expected preface and plaintext payload")
	}
	if bytes.Contains(wire, []byte(setup)) || bytes.Contains(wire, []byte(token)) {
		t.Fatal("credential or setup message appeared in plaintext")
	}
}
