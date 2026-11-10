package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTCPFWDXMPPBootstrap(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	clientWire := &recordingTCPFWDConn{Conn: client}
	serverWire := &recordingTCPFWDConn{Conn: server}
	const token = "shared-test-token-for-xmpp"
	finished := make(chan error, 1)
	go func() {
		conn, err := TCPFWDXMPPServerConn(serverWire, token, time.Second)
		if err == nil && !conn.AdaptivePool {
			err = errors.New("server did not confirm adaptive pooling")
		}
		finished <- err
	}()
	conn, err := TCPFWDXMPPClientConn(clientWire, token, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !conn.AdaptivePool {
		t.Fatal("client did not confirm adaptive pooling")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		wire  []byte
		xml   string
		hello uint32
	}{
		{clientWire.wire.Bytes(), tcpfwdXMPPClient, 62},
		{serverWire.wire.Bytes(), tcpfwdXMPPServer, 46},
	} {
		if !bytes.HasPrefix(tc.wire, []byte(tc.xml)) {
			t.Fatalf("missing XML opening %q", tc.xml)
		}
		wire := tc.wire[len(tc.xml):]
		for _, size := range []uint32{tc.hello, 37, 37} {
			if len(wire) < 4+int(size) || binary.BigEndian.Uint32(wire[:4]) != size {
				t.Fatalf("expected record size %d, got %x", size, wire)
			}
			wire = wire[4+int(size):]
		}
		if len(wire) > 0 {
			t.Fatalf("unexpected bootstrap bytes: %x", wire)
		}
	}
}

func TestTCPFWDNUM3PoolFloorIsBoundedAndKeepsTheNonce(t *testing.T) {
	nonce := strings.Repeat("a", poolNonceHexLen)
	for _, floor := range []int{1, 32, 256} {
		payload, err := EncodeNUM3Pool(nonce, floor)
		if err != nil {
			t.Fatal(err)
		}
		got, size, err := DecodeNUM3Pool(payload)
		if err != nil || got != nonce || size != floor {
			t.Fatalf("pool announcement = %q, %d, %v", got, size, err)
		}
	}
	for _, payload := range []string{nonce, nonce + "\x00\x00", nonce + "\x01\x01", nonce + "\x00\x02trailing"} {
		if _, _, err := DecodeNUM3Pool(payload); err == nil {
			t.Fatal("accepted an invalid pool announcement")
		}
	}
}

func TestTCPFWDXMPPEdgeSendsOpeningFirst(t *testing.T) {
	exit, edge := net.Pipe()
	defer exit.Close()
	defer edge.Close()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_, _ = TCPFWDXMPPServerConn(edge, "shared-test-token-for-xmpp", time.Second)
	}()
	if err := exit.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := readNUM3Opening(exit, tcpfwdXMPPServer); err != nil {
		t.Fatalf("edge did not send its opener first: %v", err)
	}
	_ = exit.Close()
	<-stopped
}

func TestTCPFWDNUM3StreamFrames(t *testing.T) {
	var wire bytes.Buffer
	aead, err := num3AEAD(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	codec := &num3Records{conn: &wire, aead: aead, dataAEAD: aead}
	tunes := 0
	sender := &num3MuxConn{codec: codec, edge: true, opening: make(map[uint32][]byte), queueTune: func() { tunes++ }}
	receiver := &num3MuxConn{codec: codec}

	const addr = "127.0.0.1:8123"
	const app = "application bytes"
	const next = "next"
	target := append([]byte{0, byte(len(addr))}, addr...)
	for _, frame := range [][]byte{
		appendSMUXFrame(nil, smuxSYN, 3, nil),
		appendSMUXFrame(nil, smuxPSH, 3, target[:1]),
		appendSMUXFrame(nil, smuxPSH, 3, append(bytes.Clone(target[1:]), app...)),
		appendSMUXFrame(nil, smuxPSH, 3, []byte(next)),
		appendSMUXFrame(nil, smuxFIN, 3, nil),
		appendSMUXFrame(nil, smuxNOP, 0, nil),
	} {
		parts := [][]byte{frame[:smuxHeaderSize], frame[smuxHeaderSize:]}
		if n, err := sender.WriteBuffers(parts); n != len(frame) || err != nil {
			t.Fatalf("writing frame %x: %d, %v", frame, n, err)
		}
	}
	if tunes != 1 {
		t.Fatalf("socket tuning ran %d times instead of once on the heartbeat", tunes)
	}

	captured := bytes.Clone(wire.Bytes())
	if bytes.Contains(captured, []byte(addr)) {
		t.Fatal("destination was visible on the wire")
	}
	if bytes.Contains(captured, []byte(app)) {
		t.Fatal("application data was visible on the wire")
	}
	open := append([]byte{1, 0, 0, 0, 1, 1, 0}, target...)
	open = append(open, 0, 0)
	for _, want := range []struct {
		body []byte
		raw  bool
	}{
		{open, false},
		{append([]byte{2, 0, 0, 0, 1}, app...), true},
		{append([]byte{2, 0, 0, 0, 1}, next...), true},
		{[]byte{3, 0, 0, 0, 1}, false},
		{[]byte{6, 0, 0, 0, 0}, false},
	} {
		got, raw, err := codec.read()
		if err == nil && raw {
			if len(got) != len(want.body)+aead.Overhead() {
				t.Fatalf("unexpected data record length %d", len(got))
			}
			if _, err := codec.openData([]byte{2, 0, 0, 0, 3}, bytes.Clone(got[5:])); err == nil {
				t.Fatal("accepted a data record on the wrong stream")
			}
			var data []byte
			data, err = codec.openData(got[:5], got[5:])
			got = append(got[:5], data...)
		}
		if err != nil || raw != want.raw || !bytes.Equal(got, want.body) {
			t.Fatalf("wire record %x, raw=%t, %v; want %x, raw=%t", got, raw, err, want.body, want.raw)
		}
	}
	if wire.Len() != 0 {
		t.Fatalf("unexpected remaining bytes: %x", wire.Bytes())
	}

	wire.Write(captured)
	expected := appendSMUXFrame(nil, smuxSYN, 3, nil)
	expected = appendSMUXFrame(expected, smuxPSH, 3, target)
	expected = appendSMUXFrame(expected, smuxPSH, 3, []byte(app))
	expected = appendSMUXFrame(expected, smuxPSH, 3, []byte(next))
	expected = appendSMUXFrame(expected, smuxFIN, 3, nil)
	expected = appendSMUXFrame(expected, smuxNOP, 0, nil)
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(receiver, actual); err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("restored stream %x, %v; want %x", actual, err, expected)
	}
}

func TestTCPFWDNUM3MaxDestination(t *testing.T) {
	var wire bytes.Buffer
	aead, err := num3AEAD(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	codec := &num3Records{conn: &wire, aead: aead}
	sender := &num3MuxConn{codec: codec, edge: true, opening: make(map[uint32][]byte)}
	receiver := &num3MuxConn{codec: codec}
	target := append([]byte{0xff, 0xff}, bytes.Repeat([]byte{'x'}, 65535)...)
	syn := appendSMUXFrame(nil, smuxSYN, 3, nil)
	frames := [][]byte{syn, appendSMUXFrame(nil, smuxPSH, 3, target[:65535]), appendSMUXFrame(nil, smuxPSH, 3, target[65535:])}
	for _, frame := range frames {
		if _, err := sender.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	expected := bytes.Join(frames, nil)
	actual := make([]byte, len(expected))
	if _, err := io.ReadFull(receiver, actual); err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("large destination round trip: %v", err)
	}
}

func TestTCPFWDNUM3RejectsReplayedControl(t *testing.T) {
	var wire bytes.Buffer
	aead, err := num3AEAD(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	codec := &num3Records{conn: &wire, aead: aead, readAAD: &[9]byte{1}, writeAAD: &[9]byte{1}}
	frame, err := codec.encode(num3Control)
	if err != nil {
		t.Fatal(err)
	}
	wire.Write(frame)
	wire.Write(frame)
	if _, _, err := codec.read(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := codec.read(); err == nil {
		t.Fatal("accepted a replayed control frame")
	}
}
