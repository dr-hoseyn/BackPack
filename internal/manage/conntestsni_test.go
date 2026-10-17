package manage

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConnectionTestSNICandidatesKeepIndependentPeerSettings(t *testing.T) {
	coord, err := startCTCoordinator(ctPickPort(map[int]bool{}, true), "sni-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer coord.close()
	link := ConnTestLink{Host: "127.0.0.1", Tok: coord.tok, Coord: coord.tcp.Addr().(*net.TCPAddr).Port,
		DirectPorts: true, SNIVariants: true}
	seen := map[string]bool{}
	for _, plan := range ctDirectPlans("") {
		if plan.carrier != "sni" {
			continue
		}
		if plan.sni == "" || seen[plan.key] {
			t.Fatal("duplicate or empty SNI candidate")
		}
		seen[plan.key] = true
		link.Cases = append(link.Cases, ShareLink{Kind: "direct", Tr: "sni", SNI: plan.sni,
			Tok: ctCaseToken(link.Tok, "direct", plan.key)})
	}
	if len(seen) != 3 {
		t.Fatalf("want three SNI candidates, got %v", seen)
	}
	coord.setConfig(ctConfig(link))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, err := fetchConnTestLink(ctx, connTestAddr{Host: link.Host, Tok: link.Tok, Coord: link.Coord})
	if err != nil {
		t.Fatal(err)
	}
	tokens, ports := map[string]bool{}, map[string]bool{}
	used := map[int]bool{}
	for i, c := range peer.Cases {
		if c.SNI != link.Cases[i].SNI || c.Tok != link.Cases[i].Tok || tokens[c.Tok] {
			t.Fatalf("peer lost a domain or reused a token: %+v", c)
		}
		tokens[c.Tok] = true
		key := ctLinkCaseKey(peer, c)
		port, err := ctKharejDirectPort(ctx, peer, key, used)
		if err != nil {
			t.Fatal(err)
		}
		if ports[port] || coord.directPort(key, "") != port {
			t.Fatal("SNI ports collided")
		}
		ports[port] = true
	}
	for _, bad := range []string{"sni@elsewhere.invalid", "sni@", "udp@www.apple.com"} {
		if got := coord.answer("port "+coord.tok+" "+bad+":9000", "127.0.0.1"); got != "" {
			t.Fatal("invalid candidate accepted")
		}
	}
	key := "sni@" + connTestSNIDomains[0]
	if coord.answer("skip "+coord.tok+" direct/"+key, "127.0.0.1") != "ok" {
		t.Fatal("candidate skip rejected")
	}
	if !coord.skipped["direct/"+key] || len(coord.skipped) != 1 {
		t.Fatal("one failed candidate affected its siblings")
	}
}

func TestConnectionTestExplicitSNIStaysASingleLegacyCase(t *testing.T) {
	count := 0
	for _, plan := range ctDirectPlans(" custom.example ") {
		if plan.carrier != "sni" {
			continue
		}
		count++
		if plan.sni != "custom.example" || plan.key != "sni" {
			t.Fatal(plan)
		}
	}
	if count != 1 {
		t.Fatal("explicit SNI expanded into unrelated candidates")
	}
	if got := ctLinkCaseKey(ConnTestLink{}, ShareLink{Kind: "direct", Tr: "sni", SNI: "custom.example"}); got != "sni" {
		t.Fatal("old test link identity changed")
	}
}

func TestConnectionTestSNIPeerUpgradeIsSkippedRatherThanReportedBlocked(t *testing.T) {
	coord := &ctCoordinator{peer: "127.0.0.1", pmtu: make(chan int, 1)}
	coord.pmtu <- 1500
	s := &ConnTestIran{coord: coord, dir: t.TempDir(), link: ConnTestLink{Host: "127.0.0.1"}}
	for _, domain := range connTestSNIDomains {
		s.cases = append(s.cases, &connTestCase{kind: "direct", tr: "sni", key: "sni@" + domain, l3: l3Spec{SNIDomain: domain}})
	}
	for _, r := range s.Run(context.Background(), nil) {
		if r.Status != ctSkipped || !strings.Contains(r.Detail, "updated Kharej") || r.SNIDomain == "" {
			t.Fatalf("old peer was reported as a failed network: %+v", r)
		}
	}
	if len(s.engines) != 0 {
		t.Fatal("an unsupported candidate engine was launched")
	}
}

func TestConnectionTestSNIRecommendationUsesSteadyTunnelTraffic(t *testing.T) {
	rows := []ConnTestResult{
		{Kind: "direct", Transport: "sni", CaseID: "sni@www.speedtest.net", SNIDomain: "www.speedtest.net", Status: ctDown, Mbps: 900},
		{Kind: "direct", Transport: "sni", CaseID: "sni@www.microsoft.com", SNIDomain: "www.microsoft.com", Status: ctOK, Mbps: 50, OK: 3, Tried: 3},
		{Kind: "direct", Transport: "sni", CaseID: "sni@www.apple.com", SNIDomain: "www.apple.com", Status: ctOK, Mbps: 90, OK: 3, Tried: 3},
		{Kind: "reverse", Transport: "ws", Status: ctOK, Mbps: 1000, OK: 3, Tried: 3},
	}
	cases := make([]*connTestCase, len(rows))
	for i := range cases {
		cases[i] = &connTestCase{name: strconv.Itoa(i)}
	}
	best := ctComputeBest(rows, cases, 0, t.TempDir())
	if best.SNIDomain != "www.apple.com" || best.Transport != "reverse ws" {
		t.Fatalf("SNI candidate or overall winner is wrong: %+v", best)
	}
	table := ConnTestTable(rows)
	for i, domain := range connTestSNIDomains {
		if !strings.Contains(table, fmt.Sprintf("SNI #%d", i+1)) || !strings.Contains(table, domain) {
			t.Fatalf("SNI result is ambiguous: %s", table)
		}
	}
	if !strings.Contains(ConnTestBestTable(best), "www.apple.com") {
		t.Fatal("recommended SNI is hidden")
	}
	rows[1].Status, rows[2].Status = ctUnstable, ctDown
	if best = ctComputeBest(rows, cases, 0, t.TempDir()); best.SNIDomain != "" {
		t.Fatal("a failed SNI candidate was recommended")
	}
}
