package e2e

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/carrier"
)

func TestSSHCarrierRunsTheFullTunnelEngine(t *testing.T) {
	backend := startEchoBackend(t)
	tunnelPort, entryPort := freePort(t), freePort(t)
	token := "test-ssh-noise-token-aaaaaaaaaaaaaa"
	server := baseServerConfig("ssh", tunnelPort, entryPort, backend.addr, token)
	client := baseClientConfig("ssh", server.BindAddr, token, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pin, err := carrier.StartTestSSH(ctx, ln, server.BindAddr, "login-password")
	if err != nil {
		t.Fatal(err)
	}
	client.CarrierOptions = config.CarrierOptions{SSHUser: "backpack-test", SSHPassword: "login-password", SSHHostKey: pin, SSHPort: ln.Addr().(*net.TCPAddr).Port}
	tun := runPair(t, server, client, entryPort, tunnelPort)
	if err := tun.waitReady(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := tun.roundTrip(randomPayload(t, 512<<10)); err != nil {
		t.Fatal(err)
	}
}
