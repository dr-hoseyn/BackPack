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
	for _, framed := range []bool{true, false} {
		t.Run(map[bool]string{true: "framed", false: "legacy"}[framed], func(t *testing.T) {
			up := websocket.Upgrader{ReadBufferSize: wsReadBufferSize, WriteBufferSize: wsWriteBufferSize, WriteBufferPool: wsWriteBuffers}
			if framed {
				up.Subprotocols = []string{WSMuxBinaryProtocol}
			}
			serverDone := make(chan error, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, e := up.Upgrade(WSMuxResponse(w), r, nil)
				if e != nil {
					serverDone <- e
					return
				}
				defer c.Close()
				s, e := smux.Server(WSMuxStream(c), nil)
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
			}))
			defer srv.Close()
			c, e := WebSocketDialer(context.Background(), nil, srv.Listener.Addr().String(), "", "/tunnel", 2*time.Second, time.Second, true, "test-token", config.WSMUX, false, 1, 0, 0, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			if (c.Subprotocol() == WSMuxBinaryProtocol) != framed {
				t.Fatal("framing disagreement")
			}
			s, e := smux.Client(WSMuxStream(c), nil)
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
