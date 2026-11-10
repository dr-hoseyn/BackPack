package network

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestTCPFWDXMPPOpeningsAndProtectedTarget(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	clientWire := &recordingTCPFWDConn{Conn: client}
	serverWire := &recordingTCPFWDConn{Conn: server}
	const token = "shared-test-token-for-xmpp"
	const target = "127.0.0.1:8123"
	const app = "application bytes on the mux data connection"
	type reply struct {
		conn *TCPFWDConn
		err  error
	}
	finished := make(chan reply, 1)
	go func() {
		conn, err := TCPFWDXMPPServerConn(serverWire, token, 2*time.Second)
		finished <- reply{conn, err}
	}()
	exit, err := TCPFWDXMPPClientConn(clientWire, token, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	edge := <-finished
	if edge.err != nil {
		t.Fatal(edge.err)
	}
	if !bytes.HasPrefix(clientWire.wire.Bytes(), []byte(tcpfwdXMPPClient)) ||
		!bytes.HasPrefix(serverWire.wire.Bytes(), []byte(tcpfwdXMPPServer)) {
		t.Fatal("the first payloads did not contain the complete XML openings")
	}
	writer, err := edge.conn.TargetWriter()
	if err != nil {
		t.Fatal(err)
	}
	reader, err := exit.TargetReader()
	if err != nil {
		t.Fatal(err)
	}
	record := writer.Seal(3, target)
	if bytes.Contains([]byte(record), []byte(target)) {
		t.Fatal("destination appeared in plaintext")
	}
	if got, err := reader.Open(3, record); err != nil || got != target {
		t.Fatalf("decoded target %q, %v", got, err)
	}
	if _, err := reader.Open(5, record); err == nil {
		t.Fatal("accepted a target record replayed on another stream")
	}
	result := make(chan error, 1)
	go func() {
		_ = edge.conn.DataConn().SetWriteDeadline(time.Now().Add(time.Second))
		_, err := io.WriteString(edge.conn.DataConn(), app)
		result <- err
	}()
	body := make([]byte, len(app))
	_ = exit.DataConn().SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(exit.DataConn(), body); err != nil || string(body) != app {
		t.Fatalf("raw data %q, %v", body, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(serverWire.wire.Bytes(), []byte(app)) {
		t.Fatal("data bytes were changed on the underlying connection")
	}
}
