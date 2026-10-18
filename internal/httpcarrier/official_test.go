package httpcarrier

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go/qlog"
)

// This is an independent protocol implementation: Backpack's clients must
// exchange bytes with the checksum-pinned upstream Rust endpoint, not just
// with their own server implementation.
func TestTrustTunnelOfficialEndpointInteroperability(t *testing.T) {
	binary := os.Getenv("BP_TRUSTTUNNEL_BINARY")
	if binary == "" {
		t.Skip("set BP_TRUSTTUNNEL_BINARY to the pinned official endpoint")
	}
	st, ct := tlsPair(t)
	target := echoTarget(t)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cert := write("cert.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: st.Certificates[0].Certificate[0]}))
	key, err := x509.MarshalPKCS8PrivateKey(st.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := write("key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	creds := write("credentials.toml", []byte("[[client]]\nusername = \"backpack\"\npassword = \"separate-secret-123456\"\n"))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	settings := write("vpn.toml", []byte(fmt.Sprintf("listen_address = %q\nallow_private_network_connections = true\ncredentials_file = %q\n[listen_protocols.http2]\n[listen_protocols.quic]\n", addr, creds)))
	hosts := write("hosts.toml", []byte(fmt.Sprintf("[[main_hosts]]\nhostname = \"carrier.backpack.invalid\"\ncert_chain_path = %q\nprivate_key_path = %q\n", cert, keyFile)))
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--loglvl", "debug", settings, hosts)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	log, err := os.Create(filepath.Join(dir, "endpoint.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { cancel(); <-done }()
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		body, _ := os.ReadFile(log.Name())
		t.Fatalf("official endpoint did not start: %s", body)
	}
	for _, mode := range []string{"h2", "h3", "auto"} {
		t.Run(mode, func(t *testing.T) {
			traceDir := t.TempDir()
			t.Setenv("QLOGDIR", traceDir)
			setup, stop := context.WithTimeout(ctx, 8*time.Second)
			defer stop()
			c, err := NewClient(ctx, "trusttunnel", mode, addr, "backpack", "separate-secret-123456", "", ct)
			if err != nil {
				t.Fatal(err)
			}
			c.h3.QUICConfig.Tracer = qlog.DefaultConnectionTracer
			defer c.Close()
			if err := exchange(setup, c, target, []byte("independent official endpoint verified bytes")); err != nil {
				c.Close()
				files, _ := filepath.Glob(filepath.Join(traceDir, "*"))
				for _, file := range files {
					body, _ := os.ReadFile(file)
					lines := strings.Split(string(body), "\n")
					t.Logf("QUIC trace tail: %s", strings.Join(lines[max(0, len(lines)-65):], "\n"))
				}
				body, _ := os.ReadFile(log.Name())
				t.Fatalf("official %s: %v\n%s", mode, err, body)
			}
		})
	}
}
