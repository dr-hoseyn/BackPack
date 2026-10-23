package host

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestTunnelCertificateSupportsVerifiedTLSAndReuse(t *testing.T) {
	name := "tunnel-tls-" + filepath.Base(filepath.Dir(t.TempDir()))
	certPath, keyPath, err := EnsureSelfSignedCert(name, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(certPath); os.Remove(keyPath) })
	if err := ValidCertPair(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureSelfSignedCert(name, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(certPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("restarting a tunnel replaced its certificate", err)
	}
	if info, err := os.Stat(keyPath); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("private key permissions: %v", info.Mode())
	}
	expiry, err := CertExpiry(certPath)
	if err != nil || time.Until(expiry) < 365*24*time.Hour {
		t.Fatal("generated tunnel certificate has no usable lifetime", expiry, err)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(before) {
		t.Fatal("cannot trust generated certificate")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		secured := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
		_, err = secured.Write([]byte("CLI tunnel"))
		done <- err
	}()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp4", ln.Addr().String(),
		&tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len("CLI tunnel"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "CLI tunnel" {
		t.Fatal("verified TLS tunnel did not carry payload", string(buf), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A missing private key must be repaired before the next tunnel starts.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureSelfSignedCert(name, "tunnel.example"); err != nil {
		t.Fatal(err)
	}
	repaired, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(repaired)
	if block == nil {
		t.Fatal("repaired certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.VerifyHostname("tunnel.example") != nil {
		t.Fatal("repaired certificate lacks the requested tunnel hostname", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatal("repaired certificate and key do not match", err)
	}
}

func TestTunnelCertificateDiagnosticsRejectMissingAndInvalidFiles(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.crt")
	missing := filepath.Join(dir, "missing.key")
	for _, data := range [][]byte{[]byte("not PEM"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")})} {
		if err := os.WriteFile(bad, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := CertExpiry(bad); err == nil {
			t.Fatal("invalid certificate reported an expiry")
		}
	}
	if _, err := CertExpiry(missing); err == nil {
		t.Fatal("missing certificate reported an expiry")
	}
	for _, files := range [][2]string{{"", bad}, {bad, ""}, {missing, bad}, {bad, missing}} {
		if err := ValidCertPair(files[0], files[1]); err == nil {
			t.Fatalf("accepted missing TLS file: %q", files)
		}
	}
}
