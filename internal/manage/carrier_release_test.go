package manage

import (
	"bytes"
	"context"
	"github.com/backpack/backpack/config"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorUsesUDPWhileTCPIsSilent(t *testing.T) {
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	go func() {
		c, e := tcp.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		io.Copy(io.Discard, c)
	}()
	go func() {
		b := make([]byte, 128)
		n, a, e := udp.ReadFrom(b)
		if e == nil {
			_, nonce, err := ctOpen("token", "request", nil, b[:n])
			if err != nil {
				return
			}
			wire, _, err := ctSeal("token", "reply", nonce, []byte("ok"))
			if err == nil {
				udp.WriteTo(wire, a)
			}
		}
	}()
	started := time.Now()
	reply, err := ctAskContext(context.Background(), "127.0.0.1", tcp.Addr().(*net.TCPAddr).Port, "hello token")
	if err != nil || reply != "ok" {
		t.Fatalf("UDP fallback failed: %q %v", reply, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("UDP waited for the TCP timeout")
	}
}

func TestCoordinatorReadsAFragmentedTCPReply(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		req, e := ctReadPacket(c)
		if e != nil {
			return
		}
		_, nonce, e := ctOpen("token", "request", nil, req)
		if e != nil {
			return
		}
		wire, _, e := ctSeal("token", "reply", nonce, []byte("config all-settings"))
		if e != nil {
			return
		}
		var framed bytes.Buffer
		if ctWritePacket(&framed, wire) != nil {
			return
		}
		b := framed.Bytes()
		c.Write(b[:7])
		time.Sleep(25 * time.Millisecond)
		c.Write(b[7:])
	}()
	reply, err := ctAsk("127.0.0.1", l.Addr().(*net.TCPAddr).Port, "config token")
	if err != nil || reply != "config all-settings" {
		t.Fatalf("truncated reply: %q %v", reply, err)
	}
}

func TestNewCarrierOptionsSurviveLinkAndRenderingWithoutSSHSecrets(t *testing.T) {
	opts := config.CarrierOptions{Fingerprint: "firefox", GRPCService: "route.Test", TLSFragment: true, SSHUser: "root", SSHPassword: "private-password", SSHKeyFile: "private-key-file", SSHHostKey: "SHA256:trusted", SSHPort: 2222}
	raw, err := (ShareLink{Kind: "reverse", From: "iran", Host: "127.0.0.1", Port: "4443", Tr: "grpc", Tok: "long-shared-token", Carrier: &opts}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	link, err := DecodeShareLink(raw)
	if err != nil {
		t.Fatal(err)
	}
	if link.Carrier == nil || link.Carrier.GRPCService != opts.GRPCService || link.Carrier.SSHPassword != "" || link.Carrier.SSHKeyFile != "" {
		t.Fatalf("carrier settings changed or leaked: %+v", link.Carrier)
	}
	spec := reverseClientFromLink(link, "127.0.0.1")
	body := spec.Render()
	if !strings.Contains(body, `grpc_service = "route.Test"`) || !strings.Contains(body, `fingerprint = "firefox"`) {
		t.Fatal("setup link did not reach the engine configuration")
	}
	if strings.Contains(body, "private-password") || strings.Contains(body, "private-key-file") {
		t.Fatal("SSH login crossed a setup link")
	}
}

func TestNewCarrierLinksOmitOnlyLocalSSHCredentials(t *testing.T) {
	for _, opts := range []config.CarrierOptions{{SSHPassword: "private-password", SSHKeyFile: "private-path"}, {}} {
		raw, err := (ShareLink{Kind: "reverse", From: "iran", Tr: "tcp", Port: "443", Tok: "tunnel-token", Carrier: &opts}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeShareLink(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got.Carrier != nil {
			t.Fatalf("local-only or empty carrier settings escaped: %+v", got.Carrier)
		}
	}
}

func TestCoordinatorKernelUDPRemainsEncryptedAndFast(t *testing.T) {
	port := ctPickPort(map[int]bool{}, true)
	c, err := startCTCoordinator(port, "kernel-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	link := ConnTestLink{Host: "127.0.0.1", Coord: port, Tok: "kernel-secret"}
	started := time.Now()
	reply, err := ctKernelAsk(context.Background(), link, "hello "+link.Tok)
	if err != nil || reply != "ok" {
		t.Fatalf("encrypted kernel exchange: %q %v", reply, err)
	}
	if time.Since(started) > 750*time.Millisecond {
		t.Fatal("kernel control waited for its old plaintext UDP timeout")
	}
}
func TestCoordinatorAdditionalCarriersKeepReverseAndDirectDNS(t *testing.T) {
	port := ctPickPort(map[int]bool{}, true)
	c, err := startCTCoordinator(port, "shared-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	link := ConnTestLink{Host: "127.0.0.1", Coord: port, Tok: "shared-secret"}
	for _, kind := range []string{"reverse", "direct"} {
		link.Cases = append(link.Cases, ShareLink{Kind: kind, Tr: "dns", From: "iran", Port: "20001"})
	}
	c.setTestConfig(link)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := fetchConnTestLink(ctx, connTestAddr{Host: link.Host, Coord: port, Tok: link.Tok})
	if err != nil || len(got.Cases) != 2 {
		t.Fatalf("DNS carrier settings collided: %d cases, %v", len(got.Cases), err)
	}
	seen := map[string]bool{}
	for _, l := range got.Cases {
		key := l.Kind + "/" + l.Tr
		if seen[key] || l.Tok != ctCaseToken(link.Tok, l.Kind, l.Tr) {
			t.Fatalf("damaged DNS settings: %+v", l)
		}
		seen[key] = true
	}
}
