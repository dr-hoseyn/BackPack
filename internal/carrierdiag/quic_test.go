package carrierdiag

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

func TestSilentUDPOnlyReportsObservation(t *testing.T) {
	l, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	s := ProbeQUIC(ctx, l.LocalAddr().String(), &tls.Config{ServerName: "localhost", NextProtos: []string{"h3"}})
	if s.Code != "no_udp_reply_observed" || s.Status != "fail" || s.SentPackets == 0 || s.ReceivedPackets != 0 {
		t.Fatalf("timeout misclassified: %+v", s)
	}
}

func TestQUICRequiresTrustedCertificateAndMatchingName(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	l, err := quic.ListenAddr("127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, NextProtos: []string{"h3"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for {
			c, err := l.Accept(ctx)
			if err != nil {
				return
			}
			context.AfterFunc(ctx, func() { c.CloseWithError(0, "") })
		}
	}()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	for _, tc := range []struct {
		name  string
		roots *x509.CertPool
		code  string
	}{
		{"localhost", roots, "verified_quic_tls"},
		{"localhost", x509.NewCertPool(), "tls_untrusted_certificate"},
		{"wrong.example", roots, "tls_name_mismatch"},
	} {
		ctx, stop := context.WithTimeout(t.Context(), 2*time.Second)
		s := ProbeQUIC(ctx, l.Addr().String(), &tls.Config{ServerName: tc.name, RootCAs: tc.roots, NextProtos: []string{"h3"}})
		stop()
		if s.Code != tc.code || s.ReceivedPackets == 0 {
			t.Fatalf("TLS probe: %+v, want %s", s, tc.code)
		}
	}
}
