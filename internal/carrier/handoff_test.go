package carrier

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
)

// Cancel at the final deadline reset, after the wire handshake has succeeded.
// Wait for a registered close callback so the old ownership race is reliable.
type cancelAtResetConn struct {
	net.Conn
	cancel context.CancelFunc
	closed chan struct{}
	once   sync.Once
}

func (c *cancelAtResetConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *cancelAtResetConn) SetDeadline(t time.Time) error {
	err := c.Conn.SetDeadline(t)
	if t.IsZero() {
		c.cancel()
		select {
		case <-c.closed:
		case <-time.After(50 * time.Millisecond):
		}
	}
	return err
}

func TestCarrierCancellationAtHandoffReturnsNoConnection(t *testing.T) {
	for _, mode := range []config.TransportType{config.TLS, config.GRPC} {
		for _, role := range []string{"client", "server"} {
			t.Run(string(mode)+"/"+role, func(t *testing.T) {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				setup, cancelSetup := context.WithCancel(ctx)
				defer cancelSetup()
				server := &Config{Mode: mode, TLS: testTLS(t)}
				done := make(chan error, 1)
				clientDone := make(chan struct{})
				go func() {
					raw, err := l.Accept()
					if err != nil {
						done <- err
						return
					}
					defer raw.Close()
					serverCtx := ctx
					if role == "server" {
						serverCtx = setup
						raw = &cancelAtResetConn{Conn: raw, cancel: cancelSetup, closed: make(chan struct{})}
					}
					c, err := server.Server(serverCtx, raw)
					if role == "client" {
						<-clientDone
					}
					if c != nil {
						c.Close()
					}
					if role == "server" && (c != nil || !errors.Is(err, context.Canceled)) {
						done <- errors.New("server handed off a connection cancelled during setup")
						return
					}
					done <- nil
				}()
				raw, err := net.Dial("tcp", l.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer raw.Close()
				clientCtx := ctx
				if role == "client" {
					clientCtx = setup
					raw = &cancelAtResetConn{Conn: raw, cancel: cancelSetup, closed: make(chan struct{})}
				}
				c, err := (&Config{Mode: mode}).Client(clientCtx, raw, l.Addr().String(), 2*time.Second)
				close(clientDone)
				if c != nil {
					defer c.Close()
				}
				if role == "client" && (c != nil || !errors.Is(err, context.Canceled)) {
					t.Errorf("client returned connection %v, error %v after setup cancellation", c != nil, err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-ctx.Done():
					t.Fatal("server did not finish")
				}
			})
		}
	}
}
