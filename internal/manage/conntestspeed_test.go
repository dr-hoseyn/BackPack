package manage

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// Every stream connects and negotiates within its own 3s limits. The varying
// payload rate makes sampling run to its maximum, exercising both complete
// directions instead of a lucky early-converging download. The old shared 20s
// budget expired during upload despite healthy, verified transfers.
func TestConnectionTestSpeedBudgetIncludesSetupForBothDirections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var servers sync.WaitGroup
	defer servers.Wait()
	result, err := ctSpeed(ctx, func(ctx context.Context) (net.Conn, error) {
		timer := time.NewTimer(2700 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
		client, server := net.Pipe()
		servers.Add(1)
		go func() {
			defer servers.Done()
			defer server.Close()
			ctServeSpeedOrEcho(&ctVariableSpeedConn{Conn: server, ctx: ctx, delayAck: true})
		}()
		return &ctVariableSpeedConn{Conn: client, ctx: ctx}, nil
	})
	if err != nil || result.download <= 0 || result.upload <= 0 || result.stable {
		t.Fatalf("healthy slow setup must complete both variable-rate directions: %+v, %v", result, err)
	}
}

type ctVariableSpeedConn struct {
	net.Conn
	ctx      context.Context
	delayAck bool
}

func (c *ctVariableSpeedConn) Write(p []byte) (int, error) {
	var delay time.Duration
	if c.delayAck && string(p) == ctSpeedAck {
		delay = 2700 * time.Millisecond
	} else if len(p) == ctSpeedBlock {
		// At least four alternating half-second samples must be observed;
		// a tenfold rate swing must never be reported as converged speed.
		delay = 10 * time.Millisecond
		if time.Now().UnixMilli()/500%2 == 0 {
			delay = 100 * time.Millisecond
		}
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-c.ctx.Done():
			return 0, c.ctx.Err()
		case <-timer.C:
		}
	}
	return c.Conn.Write(p)
}

func TestConnectionTestSpeedCancelsAnUnfinishedDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, ctSpeedStreams)
	finished := make(chan error, 1)
	go func() {
		_, err := ctSpeed(ctx, func(ctx context.Context) (net.Conn, error) {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		})
		finished <- err
	}()
	defer cancel()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("speed dials did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled speed test published a result")
		}
	case <-time.After(time.Second):
		t.Fatal("speed dials ignored the measurement context")
	}
}
