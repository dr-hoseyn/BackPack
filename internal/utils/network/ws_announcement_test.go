package network

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func announcementPair(tb testing.TB, secure, framed bool) (*websocket.Conn, *websocket.Conn, *wsTLSRecordConn) {
	tb.Helper()
	accepted := make(chan *websocket.Conn, 1)
	up := websocket.Upgrader{WriteBufferSize: wsWriteBufferSize, WriteBufferPool: wsWriteBuffers}
	if framed {
		up.Subprotocols = []string{WSMuxBinaryProtocol}
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(WSMuxResponse(w), r, nil)
		if err != nil {
			tb.Error(err)
			return
		}
		accepted <- conn
	}))
	raw := &wsTLSRecordListener{Listener: srv.Listener, accepted: make(chan *wsTLSRecordConn, 1)}
	srv.Listener = raw
	if secure {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	tb.Cleanup(srv.Close)
	dial := websocket.Dialer{WriteBufferSize: wsWriteBufferSize}
	if framed {
		dial.Subprotocols = []string{WSMuxBinaryProtocol}
	}
	if secure {
		dial.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	}
	client, _, err := dial.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		tb.Fatal(err)
	}
	server := <-accepted
	tb.Cleanup(func() { client.Close(); server.Close() })
	return server, client, <-raw.accepted
}

func announcementHeader(version, command byte, size int, id uint32) []byte {
	p := []byte{version, command, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(p[2:], uint16(size))
	binary.LittleEndian.PutUint32(p[4:], id)
	return p
}

func TestWSMuxAnnouncementJoinsOpenAndTargetWithoutChangingBytes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		for _, version := range []byte{1, 2} {
			t.Run(fmt.Sprintf("tls=%t/v%d", secure, version), func(t *testing.T) {
				server, client, raw := announcementPair(t, secure, true)
				stream := WSMuxAnnouncementStream(server).(muxVectorWriter)
				open := announcementHeader(version, 0, 0, 7)
				wantOpen := bytes.Clone(open)
				target := append([]byte{0, 9}, []byte("8443|8444")...)
				data := announcementHeader(version, 2, len(target), 7)
				before := raw.records.Load()
				if n, err := stream.WriteBuffers([][]byte{open, nil}); n != len(open) || err != nil {
					t.Fatalf("open: n=%d err=%v", n, err)
				}
				clear(open) // smux reuses its header immediately after WriteBuffers returns.
				if n, err := stream.WriteBuffers([][]byte{data, target}); n != len(data)+len(target) || err != nil {
					t.Fatalf("target: n=%d err=%v", n, err)
				}
				client.SetReadDeadline(time.Now().Add(time.Second))
				kind, got, err := client.ReadMessage()
				want := append(append(wantOpen, data...), target...)
				if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, want) {
					t.Fatalf("announcement split or corrupted: got=%x want=%x err=%v", got, want, err)
				}
				if secure && raw.records.Load()-before != 1 {
					t.Fatalf("announcement used %d TLS records, want one", raw.records.Load()-before)
				}
			})
		}
	}
}

func TestWSMuxAnnouncementFlushesBeforeInterleavedControlsAndData(t *testing.T) {
	for _, command := range []byte{0, 1, 2, 3, 4} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			server, client, _ := announcementPair(t, true, true)
			stream := WSMuxAnnouncementStream(server).(muxVectorWriter)
			open := announcementHeader(2, 0, 0, 7)
			var body []byte
			if command == 2 || command == 4 {
				body = []byte("12345678")
			}
			next := announcementHeader(2, command, len(body), 9)
			if _, err := stream.WriteBuffers([][]byte{open, nil}); err != nil {
				t.Fatal(err)
			}
			if n, err := stream.WriteBuffers([][]byte{next, body}); n != len(next)+len(body) || err != nil {
				t.Fatalf("interleaved write: %d, %v", n, err)
			}
			client.SetReadDeadline(time.Now().Add(time.Second))
			_, got, err := client.ReadMessage()
			want := append(append(bytes.Clone(open), next...), body...)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("pending open lost ordering: %x, %v", got, err)
			}
		})
	}
}

func TestWSMuxAnnouncementFailureCannotAcknowledgeAnotherOpen(t *testing.T) {
	raw := &wsControlError{}
	stream := controlServerStream(t, raw)
	stream.announceOpen = true
	open := announcementHeader(2, 0, 0, 7)
	if n, err := stream.WriteBuffers([][]byte{open, nil}); n != len(open) || err != nil {
		t.Fatalf("pending open: %d, %v", n, err)
	}
	raw.writeErr = io.ErrClosedPipe
	if _, err := stream.WriteBuffers([][]byte{announcementHeader(2, 2, 1, 7), {1}}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("physical failure hidden: %v", err)
	}
	writes := raw.writes
	if n, err := stream.WriteBuffers([][]byte{open, nil}); n != 0 || !errors.Is(err, io.ErrClosedPipe) || !raw.closed || raw.writes != writes {
		t.Fatalf("failed carrier accepted another open: n=%d err=%v closed=%v", n, err, raw.closed)
	}
}

func TestWSMuxAnnouncementCloseDiscardsOnlyPendingMetadata(t *testing.T) {
	server, _, _ := announcementPair(t, true, true)
	stream := WSMuxAnnouncementStream(server).(*webSocketStream)
	open := announcementHeader(2, 0, 0, 7)
	if _, err := stream.WriteBuffers([][]byte{open, nil}); err != nil {
		t.Fatal(err)
	}
	stream.Close()
	if n, err := stream.WriteBuffers([][]byte{open, nil}); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("closed carrier acknowledged pending metadata: %d, %v", n, err)
	}
}

func TestWSMuxAnnouncementPreservesLegacyAndGenericImmediateWrites(t *testing.T) {
	t.Run("legacy", func(t *testing.T) {
		server, _, _ := announcementPair(t, true, false)
		if WSMuxAnnouncementStream(server) != server.NetConn() {
			t.Fatal("legacy raw peer received buffered framing")
		}
	})
	t.Run("generic", func(t *testing.T) {
		server, client, _ := announcementPair(t, true, true)
		stream := WSMuxStream(server).(muxVectorWriter)
		open := announcementHeader(2, 0, 0, 7)
		if _, err := stream.WriteBuffers([][]byte{open, nil}); err != nil {
			t.Fatal(err)
		}
		client.SetReadDeadline(time.Now().Add(time.Second))
		_, got, err := client.ReadMessage()
		if err != nil || !bytes.Equal(got, open) {
			t.Fatalf("generic OpenStream now waits for application data: %x, %v", got, err)
		}
	})
}

func TestWSMuxSmallAndBulkFramesKeepTheirBoundaries(t *testing.T) {
	server, client, _ := announcementPair(t, true, true)
	stream := WSMuxAnnouncementStream(server).(muxVectorWriter)
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	for _, size := range []int{128, 240, 241, 65520, 128} {
		open := announcementHeader(2, 0, 0, 7)
		header := announcementHeader(2, 2, size, 7)
		body := bytes.Repeat([]byte{byte(size)}, size)
		want := append(append(bytes.Clone(open), header...), body...)
		if _, err := stream.WriteBuffers([][]byte{open, nil}); err != nil {
			t.Fatal(err)
		}
		if n, err := stream.WriteBuffers([][]byte{header, body}); n != len(header)+size || err != nil {
			t.Fatalf("size %d: wrote %d, error %v", size, n, err)
		}
		kind, got, err := client.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage || !bytes.Equal(got, want) {
			t.Fatalf("size %d: received %d bytes, kind %d, error %v", size, len(got), kind, err)
		}
	}
}

func BenchmarkWSMuxAnnouncementFrameWriter(b *testing.B) {
	for _, size := range []int{6, 128, 65520} {
		b.Run(fmt.Sprintf("bytes-%d", size), func(b *testing.B) {
			stream := controlServerStream(b, &wsControlSink{})
			stream.announceOpen = true
			open := [][]byte{announcementHeader(2, 0, 0, 7), nil}
			data := [][]byte{announcementHeader(2, 2, size, 7), make([]byte, size)}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := stream.WriteBuffers(open); err != nil {
					b.Fatal(err)
				}
				if _, err := stream.WriteBuffers(data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestWSSMuxRecentBulkObservesBothDirectionsAndExpires(t *testing.T) {
	server, client, _ := announcementPair(t, true, true)
	wire := WSMuxAnnouncementStream(server)
	bulk := WSMuxRecentBulk(wire)
	if bulk() || WSMuxRecentBulk(WSMuxStream(client)) != nil {
		t.Fatal("a new carrier or the client reported server bulk traffic")
	}
	from := WebSocketStream(client)
	small := []byte("light echo")
	if _, err := from.Write(small); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(wire, make([]byte, len(small))); err != nil || bulk() {
		t.Fatalf("small control traffic marked the carrier busy: %v", err)
	}
	payload := make([]byte, wsMuxBulkReadWrite)
	if _, err := from.Write(payload); err != nil {
		t.Fatal(err)
	}
	// A bulk frame may reach the mux in many short reads.
	for range 8 {
		if _, err := io.ReadFull(wire, payload[:len(payload)/8]); err != nil {
			t.Fatal(err)
		}
	}
	if !bulk() {
		t.Fatal("incoming bulk traffic was invisible to admission")
	}
	stream := wire.(*webSocketStream)
	stream.lastBulk.Store(int64(time.Since(wsMuxActivityEpoch) - wsMuxBulkActivityWindow - time.Second))
	if bulk() {
		t.Fatal("an idle carrier retained its earlier bulk penalty")
	}
	if _, err := wire.Write(payload); err != nil || !bulk() {
		t.Fatalf("outgoing bulk traffic was invisible to admission: %v", err)
	}
}

func BenchmarkWSMuxStreamAnnouncement(b *testing.B) {
	for _, coalesce := range []bool{false, true} {
		b.Run(fmt.Sprint("combined=", coalesce), func(b *testing.B) {
			server, client, raw := announcementPair(b, true, true)
			var stream muxVectorWriter = WebSocketStream(server).(*webSocketStream)
			if coalesce {
				stream = WSMuxAnnouncementStream(server).(muxVectorWriter)
			}
			open := announcementHeader(2, 0, 0, 7)
			target := append([]byte{0, 4}, []byte("8443")...)
			data := announcementHeader(2, 2, len(target), 7)
			finished := make(chan int64, 1)
			go func() { n, _ := io.Copy(io.Discard, WebSocketStream(client)); finished <- n }()
			before := raw.records.Load()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := stream.WriteBuffers([][]byte{open, nil}); err != nil {
					b.Fatal(err)
				}
				if _, err := stream.WriteBuffers([][]byte{data, target}); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(raw.records.Load()-before)/float64(b.N), "records/op")
			server.Close()
			if got, want := <-finished, int64(b.N*(len(open)+len(data)+len(target))); got != want {
				b.Fatalf("received %d bytes, want %d", got, want)
			}
		})
	}
}
