package carrier

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

func TestSSHCarrierPinsTheHostAndForwardsOnlyToTheRelay(t *testing.T) {
	cx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, e := echo.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	pin, err := StartTestSSH(cx, listener, echo.Addr().String(), "private-password")
	if err != nil {
		t.Fatal(err)
	}
	opts := config.CarrierOptions{SSHUser: "backpack-test", SSHPassword: "private-password", SSHHostKey: pin, SSHPort: listener.Addr().(*net.TCPAddr).Port}
	carrier := &Config{Mode: config.SSH, Options: opts}
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := carrier.Client(cx, raw, echo.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 5)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "hello" {
		t.Fatalf("SSH payload changed: %q %v", b, err)
	}
	raw, err = net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	carrier.Options.SSHHostKey = "SHA256:impostor"
	if _, err := carrier.Client(cx, raw, echo.Addr().String(), time.Second); err == nil {
		t.Fatal("SSH accepted an untrusted host key")
	}
}
