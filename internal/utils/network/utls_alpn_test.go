package network

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// A CDN in front of the server terminates the TLS itself, so the server's own
// ALPN pinning never gets a say — whatever the client offers is what the CDN
// may choose. This stands in for that: a listener that would rather speak h2,
// exactly as Cloudflare does.
func alpnEchoServer(t *testing.T, offers []string) net.Listener {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cdn.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"cdn.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		// The order is the point: given the choice this peer takes h2.
		NextProtos: offers,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// Inspect the serialized ClientHello, not only a server's chosen ALPN. An h1
// handshake can succeed even when the hello still advertises h2-only ALPS.
func TestWSSClientHelloALPSMatchesOfferedProtocols(t *testing.T) {
	for _, http11 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http11=%v", http11), func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			_ = server.SetDeadline(time.Now().Add(5 * time.Second))
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				defer client.Close()
				_, _ = uTLSClientConn(context.Background(), client, "cdn.example", 5*time.Second, http11)
			}()
			defer func() { server.Close(); <-finished }()
			var header [5]byte
			if _, err := io.ReadFull(server, header[:]); err != nil {
				t.Fatal(err)
			}
			record := make([]byte, 5+int(binary.BigEndian.Uint16(header[3:])))
			copy(record, header[:])
			if _, err := io.ReadFull(server, record[5:]); err != nil {
				t.Fatal(err)
			}
			spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).RawClientHello(record)
			if err != nil {
				t.Fatal(err)
			}
			offered := map[string]bool{}
			var settings []string
			for _, ext := range spec.Extensions {
				switch e := ext.(type) {
				case *utls.ALPNExtension:
					for _, protocol := range e.AlpnProtocols {
						offered[protocol] = true
					}
				case *utls.ApplicationSettingsExtension:
					settings = append(settings, e.SupportedProtocols...)
				case *utls.ApplicationSettingsExtensionNew:
					settings = append(settings, e.SupportedProtocols...)
				}
			}
			if !offered["http/1.1"] || offered["h2"] == http11 {
				t.Fatalf("unexpected ALPN: %v", offered)
			}
			for _, protocol := range settings {
				if !offered[protocol] {
					t.Fatalf("ALPS advertises %q absent from ALPN %v", protocol, offered)
				}
			}
			if !http11 && len(settings) == 0 {
				t.Fatal("full HTTPS preset lost its ALPS offer")
			}
		})
	}
}

// negotiate runs one handshake against ln and reports what the two ends agreed
// on, which is the whole contract this file is about.
func negotiate(t *testing.T, ln net.Listener, http11Only bool) (client, server string) {
	t.Helper()

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- "accept failed: " + err.Error()
			return
		}
		defer conn.Close()
		tc := conn.(*tls.Conn)
		if err := tc.HandshakeContext(context.Background()); err != nil {
			got <- "handshake failed: " + err.Error()
			return
		}
		got <- tc.ConnectionState().NegotiatedProtocol
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	uconn, err := uTLSClientConn(context.Background(), raw, "cdn.example", 10*time.Second, http11Only)
	if err != nil {
		t.Fatalf("the handshake failed (http11Only=%v): %v", http11Only, err)
	}
	defer uconn.Close()

	select {
	case server = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never completed a handshake")
	}
	return uconn.ConnectionState().NegotiatedProtocol, server
}

// The existing default preserves the full HTTPS preset. A caller can choose
// the HTTP/1.1-only fresh-WebSocket profile; these are distinct choices, not
// evidence that one is universally unidentifiable.
func TestTheFirstHelloOffersChromesOwnALPN(t *testing.T) {
	ln := alpnEchoServer(t, []string{"h2", "http/1.1"})

	_, server := negotiate(t, ln, false)
	if server != "h2" {
		t.Fatalf("a peer that prefers h2 negotiated %q — the client is no longer "+
			"offering what Chrome offers, and the hello stands out for it", server)
	}
}

// Our own server pins http/1.1, so the ordinary direct connection — which is
// most of them — never selects h2 and never needs narrowing at all.
func TestAnHTTP11OnlyPeerNeedsNoNarrowing(t *testing.T) {
	ln := alpnEchoServer(t, []string{"http/1.1"})

	client, server := negotiate(t, ln, false)
	if server != "http/1.1" || client != "http/1.1" {
		t.Fatalf("negotiated %q/%q against an http/1.1-only peer, want http/1.1", client, server)
	}
}

// The fallback, and the reason the narrowing still exists.
//
// This is the second attempt the WSS dialer makes after a peer answers the
// first one with h2. A CDN terminates the TLS itself, so the server's own ALPN
// pinning never gets a say and Cloudflare takes the h2 on offer — then answers
// the websocket upgrade with an HTTP/2 SETTINGS frame, which the HTTP/1.1
// response parser reports as
//
//	malformed HTTP response "\x00\x00\x12\x04\x00\x00\x00\x00\x00..."
//
// A websocket cannot be carried over h2 at all, so against such a peer the
// offer has to be narrowed to the one thing gorilla can speak.
func TestNarrowingTheOfferKeepsAnH2PeerOffH2(t *testing.T) {
	ln := alpnEchoServer(t, []string{"h2", "http/1.1"})

	client, server := negotiate(t, ln, true)
	if server == "h2" {
		t.Fatal("the peer still selected h2 with the offer narrowed: the websocket " +
			"upgrade that follows would be answered with an HTTP/2 SETTINGS frame")
	}
	if server != "http/1.1" || client != "http/1.1" {
		t.Fatalf("negotiated %q/%q, want http/1.1", client, server)
	}
}

// The two attempts together: what the WSS dialer does when it meets a CDN.
// The first hello is Chrome's and gets h2, which is unusable; the second is
// narrowed and gets a session a websocket can actually run over.
func TestTheDialerRecoversFromAnH2PeerOnTheSecondAttempt(t *testing.T) {
	ln := alpnEchoServer(t, []string{"h2", "http/1.1"})

	if _, server := negotiate(t, ln, false); server != "h2" {
		t.Fatalf("setup: the peer negotiated %q, wanted it to take h2", server)
	}
	client, server := negotiate(t, ln, true)
	if client != "http/1.1" || server != "http/1.1" {
		t.Fatalf("the retry negotiated %q/%q, want http/1.1 — the tunnel cannot come "+
			"up behind a CDN", client, server)
	}
}
