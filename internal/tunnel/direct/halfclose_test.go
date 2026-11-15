package direct

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// Request bodies whose end is signalled by FIN must still receive the reply.
// Exercise both forwarding bridges, the real mux, and each outer transport.
func TestTunnelReturnsResponseAfterUploadEOF(t *testing.T) {
	for _, transport := range allTransports {
		t.Run(transport, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			upload := bytes.Repeat([]byte("upload"), 8192)
			response := bytes.Repeat([]byte("response after EOF"), 8192)
			result := make(chan error, 1)
			backendDone := make(chan struct{})
			go func() {
				defer close(backendDone)
				conn, err := ln.Accept()
				if err != nil {
					result <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(4 * time.Second))
				got, err := io.ReadAll(conn)
				if err != nil || !bytes.Equal(got, upload) {
					result <- fmt.Errorf("backend upload: bytes=%d err=%v", len(got), err)
					return
				}
				_, err = conn.Write(response)
				result <- err
			}()
			t.Cleanup(func() {
				ln.Close()
				select {
				case <-backendDone:
				case <-time.After(5 * time.Second):
					t.Error("backend did not stop")
				}
			})
			tn := newTunnel(t, transport, "half-close-token", "half-close-token", ln.Addr().String())
			tn.awaitSession(t, 5*time.Second)
			conn, err := net.DialTimeout("tcp", tn.addr(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(4 * time.Second))
			if _, err := conn.Write(upload); err != nil {
				t.Fatal(err)
			}
			if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(conn)
			if err != nil || !bytes.Equal(got, response) {
				t.Fatalf("reply after upload EOF: got %d bytes, want %d; err=%v", len(got), len(response), err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
		})
	}
}
