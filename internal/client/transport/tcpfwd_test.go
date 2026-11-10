package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/utils"
	"github.com/backpack/backpack/internal/utils/network"
	"github.com/sirupsen/logrus"
)

func TestTCPFWDStopInterruptsControlAck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	const token = "test-tcpfwd-control-handshake-token"
	claim := make(chan error, 1)
	releasePeer := make(chan struct{})
	defer close(releasePeer)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			claim <- err
			return
		}
		defer conn.Close()
		setup, control, err := network.TCPFWDServerConn(conn, token, 3*time.Second)
		if err == nil && !control {
			err = fmt.Errorf("connection did not announce control setup")
		}
		if err == nil {
			_ = setup.SetReadDeadline(time.Now().Add(3 * time.Second))
			var value string
			var signal byte
			value, signal, err = utils.ReceiveBinaryTransportString(setup)
			if err == nil && (signal != utils.SG_ChanV2 || value != token) {
				err = fmt.Errorf("unexpected control claim")
			}
		}
		claim <- err
		<-releasePeer // keep the socket open without acknowledging the claim
	}()

	logger := logrus.New()
	logger.SetOutput(io.Discard)
	addr := listener.Addr().String()
	client := NewTCPClient(context.Background(), &TcpConfig{
		RemoteAddr: addr, Endpoints: network.NewEndpoints(addr), Token: token,
		DialTimeOut: 3 * time.Second, TCPFWD: true,
	}, logger)
	defer client.state.Stop()
	client.Start()

	select {
	case err := <-claim:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive the control claim")
	}

	client.state.Stop()
	stopped := make(chan struct{})
	go func() {
		client.state.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("client waited for a control acknowledgement after cancellation")
	}
}
