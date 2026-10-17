package adaptive

import (
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/socks"
)

func TestFixedSOCKSAcceptsTheEngineDialerButNeverResolvesArbitraryNames(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost", "192.0.2.1"} {
		t.Run(host, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			done := make(chan error, 1)
			targetHost := host
			if host == "localhost" || host == "192.0.2.1" {
				targetHost = "127.0.0.1"
			}
			go func() {
				err := fixedSOCKS(server, net.JoinHostPort(targetHost, "3080"))
				if err == nil {
					_, err = server.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				}
				server.Close()
				done <- err
			}()
			err := socks.NegotiateWithin(client, "", "", host, 3080, time.Second)
			serverErr := <-done
			allowed := host == "127.0.0.1" || host == "::1"
			if allowed && (err != nil || serverErr != nil) {
				t.Fatalf("engine IP negotiation refused: %v / %v", err, serverErr)
			}
			if !allowed && (err == nil || serverErr == nil) {
				t.Fatal("fixed target bypass accepted")
			}
		})
	}
}
