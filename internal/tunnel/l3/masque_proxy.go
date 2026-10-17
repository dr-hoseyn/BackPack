package l3

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/quic-go/quicvarint"
)

// proxyMasqueSocket forwards RFC 9298 context-zero UDP datagrams. An oversized
// packet is loss, not a failed CONNECT request: auto-MTU deliberately sends
// these probes, and masque-go's Proxy terminates its response on that error.
func proxyMasqueSocket(parent context.Context, w http.ResponseWriter, sock *net.UDPConn) {
	ctx, cancel := context.WithCancel(parent)
	str := w.(http3.HTTPStreamer).HTTPStream()
	w.Header().Set(http3.CapsuleProtocolHeader, "?1")
	w.WriteHeader(http.StatusOK)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			sock.Close()
			str.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
			str.CancelWrite(quic.StreamErrorCode(http3.ErrCodeNoError))
		})
	}
	stopParent := context.AfterFunc(ctx, stop)
	defer stopParent()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		defer stop()
		for {
			data, err := str.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			id, n, err := quicvarint.Parse(data)
			if err != nil || id != 0 {
				continue
			}
			if _, err := sock.Write(data[n:]); err != nil {
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		defer stop()
		buf := make([]byte, 65536)
		// Byte zero is the zero context ID; the rest is one backend datagram.
		for {
			n, err := sock.Read(buf[1:])
			if err != nil {
				return
			}
			if err := str.SendDatagram(buf[:n+1]); err != nil {
				var tooLarge *quic.DatagramTooLargeError
				if errors.As(err, &tooLarge) {
					continue
				}
				return
			}
		}
	}()
	// No optional capsule extension is negotiated; ignore the request body.
	_, _ = io.Copy(io.Discard, str)
	stop()
	workers.Wait()
}
