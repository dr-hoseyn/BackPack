package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func (p *controlledPath) transferred() map[net.Conn]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := make(map[net.Conn]int64, len(p.active))
	for conn, flow := range p.active {
		current[conn] = flow.bytes.Load()
	}
	return current
}

func TestWSSMuxReplacesOneFailedDataCarrierDuringTraffic(t *testing.T) {
	backend := startEchoBackend(t)
	tunnelPort, entryPort := freePort(t), freePort(t)
	const token = "wssmux-data-carrier-recovery-test-token"
	server := baseServerConfig("wssmux", tunnelPort, entryPort, backend.addr, token)
	path := newControlledPath(t, server.BindAddr)
	client := baseClientConfig("wssmux", path.addr, token, nil)
	const poolSize = 8
	client.ConnectionPool, client.WSMuxMaxSessions = poolSize, poolSize
	tun := runPair(t, server, client, entryPort, tunnelPort)
	if err := tun.waitReady(tunnelReadyTimeout); err != nil {
		t.Fatal(err)
	}
	waitWSSMuxRecoveryCondition(t, 5*time.Second, "initial WSSMux pool", func() bool {
		return len(path.transferred()) == poolSize+1 // including the control carrier
	})

	// Match verified user traffic to its physical carrier by observing its
	// reply through the TCP proxy. No payload inspection in the proxy.
	payload := randomPayload(t, 128<<10)
	var victim, sibling net.Conn
	var victimCarrier net.Conn
	for range poolSize {
		before := path.transferred()
		user, err := net.DialTimeout("tcp", tun.Entry, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { user.Close() })
		if err := wssmuxRecoveryEcho(user, payload); err != nil {
			t.Fatal(err)
		}
		var carrier net.Conn
		for conn, transferred := range path.transferred() {
			if transferred-before[conn] >= int64(len(payload)/2) {
				if carrier != nil {
					t.Fatal("a single reply was attributed to multiple carriers")
				}
				carrier = conn
			}
		}
		if carrier == nil {
			t.Fatal("could not identify the data carrier for a verified reply")
		}
		if victim == nil {
			victim, victimCarrier = user, carrier
		} else if carrier != victimCarrier {
			sibling = user
			break
		}
	}
	if sibling == nil {
		t.Fatal("could not establish a user on a separate data carrier")
	}

	bulkReady := make(chan error, 1)
	bulkDone := make(chan error, 1)
	bulkCtx, stopBulk := context.WithCancel(t.Context())
	defer stopBulk()
	go func() {
		err := wssmuxRecoveryEcho(sibling, payload)
		bulkReady <- err
		for err == nil {
			select {
			case <-bulkCtx.Done():
				bulkDone <- nil
				return
			default:
				err = wssmuxRecoveryEcho(sibling, payload)
			}
		}
		bulkDone <- err
	}()
	if err := <-bulkReady; err != nil {
		t.Fatal(err)
	}

	before := path.transferred()
	path.mu.Lock()
	flow := path.active[victimCarrier]
	path.mu.Unlock()
	if flow == nil {
		t.Fatal("the selected carrier ended before fault injection")
	}
	cutAt := time.Now()
	flow.stop()
	waitWSSMuxRecoveryCondition(t, 5*time.Second, "replacement WSSMux carrier", func() bool {
		active := path.transferred()
		if _, exists := active[victimCarrier]; exists || len(active) != poolSize+1 {
			return false
		}
		for conn := range active {
			if _, existed := before[conn]; !existed {
				return true
			}
		}
		return false
	})
	t.Logf("data carrier replaced after %s", time.Since(cutAt).Round(time.Millisecond))

	if err := wssmuxRecoveryEcho(victim, payload); err == nil {
		t.Fatal("the user on the cut carrier unexpectedly survived")
	}
	for range 12 {
		if err := tun.roundTrip(payload[:128]); err != nil {
			t.Fatalf("fresh user failed after data carrier recovery: %v", err)
		}
	}
	stopBulk()
	if err := <-bulkDone; err != nil {
		t.Fatalf("traffic on the healthy carrier failed: %v", err)
	}
}

func waitWSSMuxRecoveryCondition(t *testing.T, timeout time.Duration, label string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}

func wssmuxRecoveryEcho(conn net.Conn, payload []byte) error {
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := io.Copy(conn, bytes.NewReader(payload)); err != nil {
		return err
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if !bytes.Equal(payload, reply) {
		return fmt.Errorf("echo mismatch after transferring %d bytes", len(payload))
	}
	return nil
}
