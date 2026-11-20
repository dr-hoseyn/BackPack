package transport

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils/network"
	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
)

type tlsWriteCounter struct {
	net.Conn
	writes *atomic.Int64
}

func (c tlsWriteCounter) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

type tlsWriteCountingListener struct {
	net.Listener
	writes *atomic.Int64
}

func (l tlsWriteCountingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return tlsWriteCounter{Conn: conn, writes: l.writes}, nil
}

func TestWSSMuxUDPBatchReducesTLSWrites(t *testing.T) {
	upgrader := websocket.Upgrader{Subprotocols: []string{network.WSMuxBinaryProtocol}}
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
		}
		accepted <- conn
	}))
	var writes atomic.Int64
	srv.Listener = tlsWriteCountingListener{Listener: srv.Listener, writes: &writes}
	srv.StartTLS()
	defer srv.Close()
	dialer := websocket.Dialer{
		TLSClientConfig:  srv.Client().Transport.(*http.Transport).TLSClientConfig,
		Subprotocols:     []string{network.WSMuxBinaryProtocol},
		HandshakeTimeout: 3 * time.Second,
	}
	clientWS, _, err := dialer.Dial("wss"+srv.URL[len("https"):], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientWS.Close()
	serverWS := <-accepted
	defer serverWS.Close()

	cfg := smux.DefaultConfig()
	cfg.KeepAliveDisabled = true
	sender, err := smux.Client(network.WSMuxStream(serverWS), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	receiver, err := smux.Server(network.WSMuxStream(clientWS), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	_ = receiver.SetDeadline(time.Now().Add(3 * time.Second))
	out, err := sender.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	in, err := receiver.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	_ = out.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_ = in.SetReadDeadline(time.Now().Add(3 * time.Second))

	for _, batched := range []bool{false, true} {
		t.Run(fmt.Sprintf("batched=%t", batched), func(t *testing.T) {
			fw := &udpForwarder{batchReads: batched}
			flow := newUDPFlow(fw, netip.MustParseAddrPort("127.0.0.1:1234"))
			defer flow.Close()
			const payloadSize = 64
			for i := range udpReadBatchDatagrams {
				flow.deliver(bytes.Repeat([]byte{byte(i)}, payloadSize))
			}
			before := writes.Load()
			buf := make([]byte, 4096)
			for sent := 0; sent < udpReadBatchDatagrams*(payloadSize+2); {
				n, err := flow.Read(buf)
				if err != nil {
					t.Fatal(err)
				}
				written, err := out.Write(buf[:n])
				if err != nil || written != n {
					t.Fatalf("smux write returned %d/%d, %v", written, n, err)
				}
				sent += written
			}
			wantWrites := int64(udpReadBatchDatagrams)
			if batched {
				wantWrites = 1
			}
			if got := writes.Load() - before; got != wantWrites {
				t.Fatalf("raw TLS writes = %d, want %d", got, wantWrites)
			} else {
				t.Logf("raw TLS writes = %d for %d datagrams", got, udpReadBatchDatagrams)
			}
			for i := range udpReadBatchDatagrams {
				n, err := network.ReadDatagram(in, buf)
				if err != nil || !bytes.Equal(buf[:n], bytes.Repeat([]byte{byte(i)}, payloadSize)) {
					t.Fatalf("datagram %d changed: n=%d, err=%v", i, n, err)
				}
			}
		})
	}
}
