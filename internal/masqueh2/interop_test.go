package masqueh2

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// Use x/net's independently implemented client, with its Extended CONNECT
// switch confined to a subprocess rather than changing any production globals.
func TestHTTP2CapsulesInteroperateWithXNetClient(t *testing.T) {
	if os.Getenv("BP_H2_INTEROP") != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestHTTP2CapsulesInteroperateWithXNetClient$", "-test.v")
		cmd.Env = append(os.Environ(), "BP_H2_INTEROP=1", "GODEBUG=http2xconnect=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("x/net interoperability: %v\n%s", err, out)
		}
		return
	}
	serverTLS, clientTLS := testTLS(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	const target = "127.0.0.1:62101"
	const token = "a-private-token-for-independent-interop"
	s, err := Listen(ctx, "127.0.0.1:0", target, token, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(6 * time.Second))
	serverErr := make(chan error, 1)
	go func() {
		b := make([]byte, maxPacket)
		n, a, e := s.ReadFrom(b)
		if e == nil {
			_, e = s.WriteTo(b[:n], a)
		}
		serverErr <- e
	}()
	tr := &http2.Transport{TLSClientConfig: clientTLS, DisableCompression: true}
	defer tr.CloseIdleConnections()
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()
	path, _ := targetPath(target)
	req, err := http.NewRequestWithContext(ctx, "CONNECT", "https://"+s.LocalAddr().String()+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(":protocol", "connect-udp")
	req.Header.Set("Capsule-Protocol", "?1")
	req.Header.Set("Authorization", authorization(token))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Capsule-Protocol") != "?1" {
		t.Fatalf("CONNECT response %s", resp.Status)
	}
	want := bytes.Repeat([]byte("independent capsule framing;"), 1000)
	wire := datagramCapsule(want)
	sent := make(chan error, 1)
	go func() { _, err := w.Write(wire); sent <- err }()
	got := make([]byte, len(wire))
	if _, err = io.ReadFull(resp.Body, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wire) {
		t.Fatal("x/net and carrier disagreed about Capsule framing")
	}
	if err = <-sent; err != nil {
		t.Fatal(err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}
