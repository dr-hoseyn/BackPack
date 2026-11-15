package network

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

type capturedNoiseWire struct {
	net.Conn
	wire bytes.Buffer
}

type noiseFrameInput struct {
	net.Conn
	reader bytes.Reader
}

func (s *noiseFrameInput) Read(p []byte) (int, error) { return s.reader.Read(p) }

// Compare only framing overhead; encryption, network speed and handshakes are
// deliberately outside this microbenchmark. Both paths reuse their storage.
func BenchmarkNoiseRecordFraming(b *testing.B) {
	for _, size := range []int{64, 4096, noiseMaxFrame} {
		wire := make([]byte, noiseLenPrefix+size)
		binary.BigEndian.PutUint16(wire, uint16(size))
		for _, resumable := range []bool{false, true} {
			b.Run(fmt.Sprintf("bytes=%d/resumable=%v", size, resumable), func(b *testing.B) {
				input := &noiseFrameInput{}
				conn := &noiseConn{Conn: input, inBuf: make([]byte, size)}
				buf := make([]byte, size)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					input.reader.Reset(wire)
					var err error
					if resumable {
						_, err = conn.readFrame()
					} else {
						buf, err = readNoiseFrameInto(input, buf)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func (c *capturedNoiseWire) Write(p []byte) (int, error) { return c.wire.Write(p) }

func TestNoiseReadResumesPartialRecordAfterTimeout(t *testing.T) {
	for _, padded := range []bool{false, true} {
		for _, split := range []int{1, 2, 3, 19} {
			t.Run(fmt.Sprintf("padding=%v/split=%d", padded, split), func(t *testing.T) {
				client, server, cerr, serr := noisePair(t, "deadline-test-token", "deadline-test-token")
				if cerr != nil || serr != nil {
					t.Fatalf("handshake: %v / %v", cerr, serr)
				}
				defer client.Close()
				defer server.Close()
				writer, reader := client.(*noiseConn), server.(*noiseConn)
				writer.pad, reader.pad = padded, padded
				raw := writer.Conn
				capture := &capturedNoiseWire{Conn: raw}
				writer.Conn = capture
				payload := bytes.Repeat([]byte("resume encrypted data"), 10)
				if _, err := writer.Write(payload); err != nil {
					t.Fatal(err)
				}
				writer.Conn = raw
				wire := capture.wire.Bytes()
				if _, err := raw.Write(wire[:split]); err != nil {
					t.Fatal(err)
				}
				if err := reader.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
					t.Fatal(err)
				}
				buf := make([]byte, len(payload))
				n, err := reader.Read(buf)
				if ne, ok := err.(net.Error); n != 0 || !ok || !ne.Timeout() {
					t.Fatalf("partial record read: %d, %v", n, err)
				}
				if err := reader.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := raw.Write(wire[split:]); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(reader, buf); err != nil {
					t.Fatal("resume:", err)
				}
				if !bytes.Equal(buf, payload) {
					t.Fatal("resumed record was corrupted")
				}
				// A subsequent record also needs the correct cipher nonce and boundary.
				if _, err := writer.Write(payload); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(reader, buf); err != nil {
					t.Fatal("next record:", err)
				}
				if !bytes.Equal(buf, payload) {
					t.Fatal("next record was corrupted")
				}
			})
		}
	}
}
