package network

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
)

func TestWSMuxBinaryStreamAndLegacyCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   config.TransportType
		framed bool
	}{
		{"framed WS", config.WSMUX, true},
		{"legacy WS", config.WSMUX, false},
		{"framed WSS", config.WSSMUX, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const token = "test-token"
			up := websocket.Upgrader{ReadBufferSize: wsReadBufferSize, WriteBufferSize: wsWriteBufferSize, WriteBufferPool: wsWriteBuffers}
			if tc.framed {
				up.Subprotocols = []string{WSMuxBinaryProtocol}
			}
			serverDone := make(chan error, 1)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headers := make(http.Header)
				if tc.mode == config.WSSMUX {
					proof, err := WSSServerAnswer(r.TLS, token)
					if err != nil {
						serverDone <- err
						return
					}
					headers.Set(WSSServerProofHeader, proof)
				}
				c, e := up.Upgrade(WSMuxResponse(w), r, headers)
				if e != nil {
					serverDone <- e
					return
				}
				defer c.Close()
				s, e := NewWatchedMuxSession(WSMuxStream(c), nil, smux.Server)
				if e != nil {
					serverDone <- e
					return
				}
				defer s.Close()
				stream, e := s.AcceptStream()
				if e != nil {
					serverDone <- e
					return
				}
				_, e = io.Copy(stream, stream)
				serverDone <- e
			})
			var srv *httptest.Server
			var options []WSDialOption
			if tc.mode == config.WSSMUX {
				srv = httptest.NewTLSServer(handler)
				options = append(options, trustTestWSS(t, srv))
			} else {
				srv = httptest.NewServer(handler)
			}
			defer srv.Close()
			c, e := WebSocketDialer(context.Background(), nil, srv.Listener.Addr().String(), "", "/tunnel", 2*time.Second, time.Second, true, token, tc.mode, false, 1, 0, 0, 0, options...)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			if (c.Subprotocol() == WSMuxBinaryProtocol) != tc.framed {
				t.Fatal("framing disagreement")
			}
			s, e := NewWatchedMuxSession(WSMuxStream(c), nil, smux.Client)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			stream, e := s.OpenStream()
			if e != nil {
				t.Fatal(e)
			}
			stream.SetDeadline(time.Now().Add(5 * time.Second))
			payload := make([]byte, 1<<20)
			if _, e = rand.Read(payload); e != nil {
				t.Fatal(e)
			}
			writeDone := make(chan error, 1)
			go func() { _, e := stream.Write(payload); writeDone <- e }()
			got := make([]byte, len(payload))
			if _, e = io.ReadFull(stream, got); e != nil {
				t.Fatal(e)
			}
			if e = <-writeDone; e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("corrupt framed stream")
			}
			stream.Close()
			s.Close()
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("server stream leaked")
			}
		})
	}
}

func TestWebSocketStreamHandlesMessageBoundariesAndPing(t *testing.T) {
	up := websocket.Upgrader{Subprotocols: []string{WSMuxBinaryProtocol}, WriteBufferSize: 128}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := up.Upgrade(WSMuxResponse(w), r, nil)
		if e != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.BinaryMessage, nil)
		c.WriteControl(websocket.PingMessage, []byte("alive"), time.Now().Add(time.Second))
		wtr, e := c.NextWriter(websocket.BinaryMessage)
		if e != nil {
			return
		}
		wtr.Write(bytes.Repeat([]byte("x"), 1024))
		wtr.Close()
		c.WriteMessage(websocket.BinaryMessage, []byte("tail"))
		c.SetReadDeadline(time.Now().Add(time.Second))
		c.ReadMessage()
	}))
	defer srv.Close()
	d := websocket.Dialer{Subprotocols: []string{WSMuxBinaryProtocol}}
	c, _, e := d.Dial("ws://"+srv.Listener.Addr().String(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	stream := WebSocketStream(c)
	stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, e := stream.Read(nil); n != 0 || e != nil {
		t.Fatal("zero-length read")
	}
	got := make([]byte, 1028)
	if _, e = io.ReadFull(stream, got); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, append(bytes.Repeat([]byte("x"), 1024), []byte("tail")...)) {
		t.Fatal("message boundaries lost bytes")
	}
}

func TestWSMuxStreamDeadlineInterruptsBlockedFrame(t *testing.T) {
	release := make(chan struct{})
	up := websocket.Upgrader{Subprotocols: []string{WSMuxBinaryProtocol}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := up.Upgrade(WSMuxResponse(w), r, nil)
		if e != nil {
			return
		}
		defer c.Close()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	c, e := WebSocketDialer(context.Background(), nil, srv.Listener.Addr().String(), "", "/tunnel", time.Second, time.Second, true, "token", config.WSMUX, false, 1, 0, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	stream := WSMuxStream(c)
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<10)
		for {
			if _, e := stream.Write(buf); e != nil {
				done <- e
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if e = stream.SetWriteDeadline(time.Now().Add(50 * time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		var ne net.Error
		if !errors.As(e, &ne) || !ne.Timeout() {
			t.Fatalf("blocked frame: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("framing reset the stream's write deadline")
	}
}

func TestWebSocketStreamKeepsTheFirstTerminalReadError(t *testing.T) {
	for _, badFrame := range []bool{true, false} {
		name := "peer closed"
		if badFrame {
			name = "unexpected text followed by binary"
		}
		t.Run(name, func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if badFrame {
					_ = conn.WriteMessage(websocket.TextMessage, []byte("wrong frame type"))
					_ = conn.WriteMessage(websocket.BinaryMessage, []byte("must not reach the stream"))
				}
			}))
			defer srv.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws://"+srv.Listener.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			stream := WebSocketStream(conn)
			defer stream.Close()
			stream.SetReadDeadline(time.Now().Add(2 * time.Second))
			var p [32]byte
			n, first := stream.Read(p[:])
			if n != 0 || first == nil || (badFrame && !errors.Is(first, errWSBinaryRequired)) {
				t.Fatalf("unexpected initial read: %d, %v", n, first)
			}
			for i := range 1100 {
				if n, err := stream.Read(p[:]); n != 0 || err != first {
					t.Fatalf("read %d after failure returned %d, %v; want original error %v", i, n, err, first)
				}
			}
		})
	}
}

func TestWSMuxScatterGatherPreservesCallerBuffers(t *testing.T) {
	header := []byte("12345678")
	payload := bytes.Repeat([]byte("p"), 65535)
	want := append(append([]byte{}, header...), payload...)
	received := make(chan []byte, 1)
	up := websocket.Upgrader{Subprotocols: []string{WSMuxBinaryProtocol}, WriteBufferSize: wsWriteBufferSize}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := up.Upgrade(WSMuxResponse(w), r, nil)
		if e != nil {
			return
		}
		defer c.Close()
		_, p, e := c.ReadMessage()
		if e == nil {
			received <- p
		}
	}))
	defer srv.Close()
	c, e := WebSocketDialer(context.Background(), nil, srv.Listener.Addr().String(), "", "/tunnel", time.Second, time.Second, true, "token", config.WSMUX, false, 1, 0, 0, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	stream := WSMuxStream(c).(*webSocketStream)
	if n, e := stream.WriteBuffers(nil); n != 0 || e != nil {
		t.Fatal("empty vector was not a no-op")
	}
	if n, e := stream.WriteBuffers([][]byte{header, nil, payload}); e != nil || n != len(want) {
		t.Fatalf("vector write: %d, %v", n, e)
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, want) {
			t.Fatal("vector changed message bytes")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("vector message not received")
	}
	if !bytes.Equal(header, []byte("12345678")) || !bytes.Equal(payload, bytes.Repeat([]byte("p"), 65535)) {
		t.Fatal("masking modified caller-owned data")
	}
}
