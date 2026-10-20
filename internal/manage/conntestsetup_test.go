package manage

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func ctSetupFixture() (ConnTestLink, []ConnTestResult) {
	l := ConnTestLink{Host: "192.0.2.1", Preset: PresetTurbo, Cases: []ShareLink{
		{Kind: "reverse", From: "iran", Name: "ct-original", Tr: "kcp", Port: "45001", Tok: "test-only",
			Preset: PresetTurbo, Ports: "127.0.0.1:1234=127.0.0.1:5678", FECData: 10, FECParity: 3},
		{Kind: "direct", From: "iran", Name: "ct-direct", Tr: "sni", Port: "45002", Tok: "direct-test",
			Preset: PresetBalance, SNI: "www.example.com", LocalIP: "10.234.8.1/30", PeerIP: "10.234.8.2", MTU: 1280},
	}}
	return l, []ConnTestResult{
		{Kind: "reverse", Transport: "kcp", Status: ctOK},
		{Kind: "direct", Transport: "sni", SNIDomain: "www.example.com", Status: ctOK},
		{Kind: "spoof", Transport: ctSpoofToIran, Status: ctOK},
		{Kind: "reverse", Transport: "tcp", Status: ctDown},
	}
}

func TestConnTestSetupPairsTestedSettingsAndReplacesEchoCredentials(t *testing.T) {
	l, results := ctSetupFixture()
	s := newCTSetup(l, "198.51.100.2", results)
	for _, kind := range []string{"reverse", "direct"} {
		tr := "kcp"
		if kind == "direct" {
			tr = "sni"
		}
		r := ConnTestSetupRequest{Kind: kind, Transport: tr, Ports: "443=127.0.0.1:2096", AcceptUDP: true}
		if kind == "direct" {
			r.SNIDomain = "www.example.com"
		}
		iran, kharej, err := s.forms(r)
		if err != nil {
			t.Fatal(err)
		}
		if iran.Token != kharej.Token || len(iran.Token) != 64 || iran.Token == l.Cases[0].Tok || iran.Token == l.Cases[1].Tok {
			t.Fatal("the permanent pair must share a fresh credential")
		}
		if iran.Ports != r.Ports || !iran.AcceptUDP || kharej.Ports != "" {
			t.Fatalf("ports belong to Iran: %+v / %+v", iran, kharej)
		}
		if iran.TunnelPort != kharej.TunnelPort || strings.HasPrefix(iran.Name, "ct-") || iran.Name == kharej.Name {
			t.Fatal("the pair must use its tested port and fresh side-specific names")
		}
		if kind == "reverse" {
			if kharej.ServerAddr != l.Host || iran.FECData != 10 || kharej.FECParity != 3 || iran.Transport != "kcp" {
				t.Fatal("reverse dial address or tested FEC was changed")
			}
		} else if iran.ServerAddr != s.peer || hostOf(iran.LocalIP) != kharej.PeerIP ||
			hostOf(kharej.LocalIP) != iran.PeerIP || iran.SNIDomain != "www.example.com" || iran.MTU != 1280 {
			t.Fatalf("direct addresses, SNI or MTU changed: %+v / %+v", iran, kharej)
		}
	}
}

func TestConnTestSetupRefusesUntestedPortsExpiredAndDuplicateRequests(t *testing.T) {
	l, results := ctSetupFixture()
	r := ConnTestSetupRequest{ID: "one", Kind: "reverse", Transport: "kcp", Ports: "443"}
	s := newCTSetup(l, "198.51.100.2", results)
	if err := s.Start(r); err == nil {
		t.Fatal("accepted a request without a ready kharej")
	}
	s.answer("setup-poll", "")
	for _, bad := range []ConnTestSetupRequest{
		{ID: "bad", Kind: "reverse", Transport: "tcp", Ports: "443"},
		{ID: "bad", Kind: "spoof", Transport: ctSpoofToIran, Ports: "443"},
		{ID: "bad", Kind: "reverse", Transport: "kcp", Ports: "65536"},
		{ID: "bad", Kind: "reverse", Transport: "kcp", Ports: ""},
	} {
		if err := s.Start(bad); err == nil {
			t.Errorf("accepted invalid request: %+v", bad)
		}
	}
	if err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(r); err != nil {
		t.Fatalf("retry of an identical request must be idempotent: %v", err)
	}
	r.Ports = "8443"
	if err := s.Start(r); err == nil {
		t.Fatal("reused an ID with different settings")
	}
	s = newCTSetup(l, "198.51.100.2", results)
	s.answer("setup-poll", "")
	s.v.Expires = time.Now().Add(-time.Second).Unix()
	if err := s.Start(r); err == nil {
		t.Fatal("accepted an expired session")
	}
}

// A real TCP/UDP coordinator connects two agents with fake service operations.
// No systemd, root privileges or production hosts are used by these tests.
func TestConnTestAutomaticSetupAcrossCoordinator(t *testing.T) {
	for _, scenario := range []string{"iran", "kharej", "peer-fails", "verify-fails"} {
		t.Run(scenario, func(t *testing.T) {
			l, results := ctSetupFixture()
			s := newCTSetup(l, "198.51.100.2", results)
			c, err := startCTCoordinator(ctPickPort(map[int]bool{}, true), "secret")
			if err != nil {
				t.Fatal(err)
			}
			defer c.close()
			c.mu.Lock()
			c.setup = s
			c.mu.Unlock()
			addr := connTestAddr{Host: "127.0.0.1", Coord: c.tcp.Addr().(*net.TCPAddr).Port, Tok: "secret"}
			var mu sync.Mutex
			created, removed := map[string]int{}, map[string]int{}
			ops := ctSetupOps{
				validate: func(PeerForm) error { return nil },
				apply: func(f PeerForm) (string, bool, error) {
					mu.Lock()
					created[f.Side]++
					mu.Unlock()
					if scenario == "peer-fails" && f.Side == "kharej" {
						return "", false, errors.New("occupied tunnel port")
					}
					return "backpack-" + f.Name, true, nil
				},
				remove: func(f PeerForm) error {
					mu.Lock()
					defer mu.Unlock()
					removed[f.Name]++
					return nil
				},
				verify: func(context.Context, string) error {
					if scenario == "verify-fails" {
						return errors.New("peer never connected")
					}
					return nil
				},
			}
			s.ops = ops
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			go s.Run(ctx)
			peerDone := make(chan error, 1)
			go func() { peerDone <- ctRunSetupPeer(ctx, addr, ops, nil) }()
			for !s.View().Ready && ctx.Err() == nil {
				ctSleep(ctx, 10*time.Millisecond)
			}
			r := ConnTestSetupRequest{ID: "retry-this-id", Kind: "reverse", Transport: "kcp", Ports: "443"}
			if scenario == "kharej" {
				if reply := c.answer("setup-start secret "+ctSetupEncode(r), "198.51.100.2"); !strings.HasPrefix(reply, "setup ") {
					t.Fatal(reply)
				}
			} else if err := s.Start(r); err != nil {
				t.Fatal(err)
			}
			if err := s.Start(r); err != nil {
				t.Fatal("lost-reply retry:", err)
			}
			select {
			case <-s.done:
			case <-ctx.Done():
				t.Fatal("deployment timed out")
			}
			peerErr := <-peerDone
			failed := scenario == "peer-fails" || scenario == "verify-fails"
			if (peerErr != nil) != failed || (s.View().State == "failed") != failed {
				t.Fatalf("unexpected result: %+v; peer: %v", s.View(), peerErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if created["iran"] != 1 || created["kharej"] != 1 {
				t.Fatalf("each end must be created once: %+v", created)
			}
			if failed && (removed[s.View().Iran] != 1 || removed[s.View().Kharej] != 1) {
				t.Fatalf("failed pair was not rolled back: %+v", removed)
			}
			if !failed && len(removed) != 0 {
				t.Fatal("a successful tunnel was removed")
			}
		})
	}
}

func TestConnTestSetupProtocolRequiresTokenAndCurrentAcknowledgement(t *testing.T) {
	l, rows := ctSetupFixture()
	s := newCTSetup(l, "198.51.100.2", rows)
	c := &ctCoordinator{tok: "secret", setup: s}
	if reply := c.answer("setup-poll wrong", "198.51.100.2"); reply != "" || s.View().Ready {
		t.Fatal("unauthenticated poll must not make the kharej ready")
	}
	c.answer("setup-poll secret", "198.51.100.2")
	r := ConnTestSetupRequest{ID: "current", Kind: "reverse", Transport: "kcp", Ports: "443"}
	if err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	ack := ctSetupAck{ID: "old", Stage: "prepared"}
	if reply := c.answer("setup-ack secret "+ctSetupEncode(ack), "198.51.100.2"); !strings.HasPrefix(reply, "error ") {
		t.Fatal("accepted an acknowledgement for another deployment")
	}
	if s.ack.Stage != "" {
		t.Fatal("stale acknowledgement changed the state")
	}
	if strings.Contains(ctSetupEncode(s.View()), "test-only") {
		t.Fatal("credentials escaped into the public view")
	}
}

func TestConnTestSetupCancellationRollsBackOnlyItsNewLocalEnd(t *testing.T) {
	l, rows := ctSetupFixture()
	s := newCTSetup(l, "198.51.100.2", rows)
	s.answer("setup-poll", "")
	r := ConnTestSetupRequest{ID: "cancel", Kind: "reverse", Transport: "kcp", Ports: "443"}
	if err := s.Start(r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var removed PeerForm
	s.ops = ctSetupOps{
		validate: func(PeerForm) error { return nil },
		apply: func(PeerForm) (string, bool, error) {
			cancel() // local service was written, peer has not applied yet
			return "new-service", true, nil
		},
		remove: func(f PeerForm) error { removed = f; return nil },
	}
	s.answer("setup-ack", ctSetupEncode(ctSetupAck{ID: r.ID, Stage: "prepared"}))
	s.Run(ctx)
	if s.View().State != "failed" || removed.Name != s.View().Iran || removed.Token == "" {
		t.Fatalf("cancelled setup left its local end behind: %+v / %+v", s.View(), removed)
	}
}
