package manage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/backpack/backpack/internal/manage/backup"
	"github.com/backpack/backpack/internal/testport"
	"github.com/backpack/backpack/internal/tunnel/direct"
	"github.com/backpack/backpack/internal/updatehealth"
	"github.com/sirupsen/logrus"
)

func TestUpdateHealthRequiresTheNamedTunnelsLocalForwardedEntry(t *testing.T) {
	check := updatehealth.Check{Tunnel: "edge", URL: "http://127.0.0.1:18080/health", ExpectedBody: "ok"}
	for _, test := range []struct {
		name, transport, role, mapping string
		want                           bool
	}{
		{"reverse", "tcp", "server", "18080=8080", true},
		{"raw-udp-has-no-tcp-entry", "udp", "server", "18080=8080", false},
		{"kcp-has-stream-entry", "kcp", "server", "18080=8080", true},
		{"reverse-client", "tcp", "client", "18080=8080", false},
		{"direct-edge", "direct/tcp", "iran", "127.0.0.1:18080=8080", true},
		{"direct-origin", "direct/tcp", "kharej", "18080=8080", false},
		{"l3-dial", "l3/udp", "iran", "18080=8080", true},
		{"l3-listen", "l3/quic", "kharej", "18080=8080", true},
		{"awg", "awg", "client", "18080=8080", true},
		{"wrong-port", "tcp", "server", "18081=8080", false},
		{"non-loopback-bind", "tcp", "server", "192.0.2.1:18080=8080", false},
		{"different-loopback", "tcp", "server", "127.0.0.2:18080=8080", false},
		{"range", "tcp", "server", "18079-18081=8080-8082", true},
		{"invalid-mapping", "tcp", "server", "not-a-port", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tunnel := Tunnel{Name: "edge", Transport: test.transport, Role: test.role, Ports: []string{test.mapping}}
			err := validateUpdateHealthRoutes(updatehealth.Policy{Checks: []updatehealth.Check{check}}, []Tunnel{tunnel})
			if (err == nil) != test.want {
				t.Fatalf("validation = %v, want accepted=%v", err, test.want)
			}
		})
	}
	if validateUpdateHealthRoutes(updatehealth.Policy{Checks: []updatehealth.Check{check}}, nil) == nil {
		t.Fatal("a stale policy naming a removed tunnel was accepted")
	}
}

// This starts actual authenticated Direct tunnels and transfers a 32 KiB HTTP
// body through their forwarding/mux data path. Then only the origin is stopped:
// the entry listener and the updater's systemd checks still report success.
// That was the false-positive that previously kept a broken update installed.
func TestUpdateHealthRollsBackAnActiveServiceWithABrokenRealTunnel(t *testing.T) {
	for _, carrier := range []string{direct.TransportTCP, direct.TransportStealth} {
		t.Run(carrier, func(t *testing.T) {
			body := strings.Repeat("backpack update canary\n", 1500)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }))
			defer backend.Close()
			logger := logrus.New()
			logger.SetOutput(io.Discard)
			origin, err := direct.NewOrigin(direct.Config{Addr: "127.0.0.1:0", Token: "update-health-test-token", Transport: carrier}, logger)
			if err != nil {
				t.Fatal(err)
			}
			originCtx, stopOrigin := context.WithCancel(context.Background())
			originDone := make(chan struct{})
			go func() { defer close(originDone); _ = origin.Run(originCtx) }()
			t.Cleanup(func() { stopOrigin(); <-originDone })
			for deadline := time.Now().Add(5 * time.Second); origin.LocalAddr() == nil; {
				if time.Now().After(deadline) {
					t.Fatal("origin did not bind")
				}
				time.Sleep(10 * time.Millisecond)
			}
			port := testport.Free(t)
			mapping := fmt.Sprintf("127.0.0.1:%d=%s", port, strings.TrimPrefix(backend.URL, "http://"))
			edge, err := direct.NewEdge(direct.Config{Addr: origin.LocalAddr().String(), Token: "update-health-test-token", Transport: carrier,
				Ports: []string{mapping}, RetryDelay: 50 * time.Millisecond, DialTimeout: 200 * time.Millisecond}, logger)
			if err != nil {
				t.Fatal(err)
			}
			edgeCtx, stopEdge := context.WithCancel(context.Background())
			edgeDone := make(chan struct{})
			go func() { defer close(edgeDone); _ = edge.Run(edgeCtx) }()
			t.Cleanup(func() { stopEdge(); <-edgeDone })
			check := updatehealth.Check{Tunnel: "edge", URL: fmt.Sprintf("http://127.0.0.1:%d/health", port), ExpectedBody: body}
			policy := updatehealth.Policy{GraceSeconds: 1, Checks: []updatehealth.Check{check}}
			if err := validateUpdateHealthRoutes(policy, []Tunnel{{Name: "edge", Role: "iran", Transport: "direct/" + carrier, Ports: []string{mapping}}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if bad := updatehealth.Verify(ctx, policy.Checks, true); len(bad) != 0 {
				t.Fatalf("baseline did not cross real tunnel: %+v", bad)
			}
			snapshot := backup.Snapshot{Dir: "test-restore-point", Meta: backup.SnapshotMeta{Version: "previous-version"}}
			restored := 0
			ops := updateHealthOps{
				services: func(skip []string) []string {
					if len(skip) != 1 || skip[0] != "calling-panel.service" {
						t.Error("lost deferred restart list")
					}
					return nil // service is active before and after the cut
				},
				restore: func(s backup.Snapshot, _ func(string)) error {
					if s.Dir != snapshot.Dir {
						t.Error("restored the wrong snapshot")
					}
					restored++
					return nil
				},
			}
			var logs []string
			logf := func(line string) { logs = append(logs, line) }
			if err := finishUpdateHealthWith(snapshot, policy, []string{"calling-panel.service"}, logf, ops); err != nil || restored != 0 {
				t.Fatalf("working tunnel rolled back: %v", err)
			}
			stopOrigin()
			<-originDone
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
			if err != nil {
				t.Fatalf("entry listener stopped too, test needs active service: %v", err)
			}
			conn.Close()
			if err := updatehealth.Probe(context.Background(), updatehealth.Check{URL: backend.URL, ExpectedBody: body}); err != nil {
				t.Fatalf("backend itself failed, test needs only the tunnel cut: %v", err)
			}
			logs = nil
			err = finishUpdateHealthWith(snapshot, policy, []string{"calling-panel.service"}, logf, ops)
			if restored != 1 || err == nil || !strings.Contains(err.Error(), "payload verification") || !strings.Contains(err.Error(), "previous-version") {
				t.Fatalf("broken tunnel kept despite active service: restores=%d error=%v", restored, err)
			}
			if strings.Contains(strings.Join(logs, "\n"), "health checks passed") {
				t.Fatal("reported health success after rollback")
			}
		})
	}
}

func TestUpdateHealthServiceFailureAndRollbackFailureRemainVisible(t *testing.T) {
	sentinel := errors.New("restore disk failure")
	called := false
	err := finishUpdateHealthWith(backup.Snapshot{Dir: "/test/snapshot"}, updatehealth.Policy{}, nil, func(string) {}, updateHealthOps{
		services: func([]string) []string { return []string{"monitor"} },
		restore: func(backup.Snapshot, func(string)) error {
			called = true
			return sentinel
		},
	})
	if !called || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), "/test/snapshot") || !strings.Contains(err.Error(), "monitor") {
		t.Fatalf("rollback failure hidden: %v", err)
	}
}

func TestUpdateHealthBaselineFailureStopsInstallationAndUnconfiguredTrafficIsNotClaimed(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "existing backend fault") }))
	defer s.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	tunnels := []Tunnel{{Name: "edge", Transport: "tcp", Role: "server", Ports: []string{port + "=8080"}}}
	policy := updatehealth.Policy{Checks: []updatehealth.Check{{Tunnel: "edge", URL: s.URL, ExpectedBody: "healthy"}}}
	var logs []string
	logf := func(s string) { logs = append(logs, s) }
	if _, err := prepareUpdateHealthWith(policy, tunnels, logf); err == nil || !strings.Contains(err.Error(), "update not installed") {
		t.Fatalf("pre-existing outage was not rejected before installation: %v", err)
	}
	logs = nil
	if _, err := prepareUpdateHealthWith(updatehealth.Policy{}, tunnels, logf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "no payload check configured") {
		t.Fatal("unconfigured tunnel was not identified as unverified")
	}
}
