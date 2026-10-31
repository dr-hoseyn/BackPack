package manage

import (
	"bytes"
	stdgzip "compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConnectionTestCompactionPreservesPeerSettings(t *testing.T) {
	link := ConnTestLink{Host: "203.0.113.10", Tok: "coordinator-secret", Coord: 43210,
		Cases: []ShareLink{
			{Kind: "reverse", From: "iran", Name: "ct-http", Tok: "case-secret", Tr: "trusttunnel", Port: "443",
				Host: "203.0.113.10", Ports: "12345=127.0.0.1:23456", MSS: 1200, MuxVer: 2,
				HelperMode: "auto", HelperSNI: "peer.example", HelperUser: "account", HelperPassword: "separate-secret-123456"},
			{Kind: "direct", Tok: "other-secret", Tr: "webtransport", Host: "203.0.113.10", Port: "8443", Ports: "9000=127.0.0.1:9001", MTU: 1280},
		}}
	original := append([]ShareLink(nil), link.Cases...)
	body, err := unGzipB64(ctConfig(link))
	if err != nil {
		t.Fatal(err)
	}
	var peer ConnTestLink
	if err := json.Unmarshal(body, &peer); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(link.Cases, original) {
		t.Fatal("serializing the peer settings changed the running Iran cases")
	}
	if peer.Host != "" || peer.Tok != "" || peer.Coord != 0 {
		t.Fatal("repeated coordinator settings consumed the packet budget")
	}
	expected := append([]ShareLink(nil), original...)
	for i := range expected {
		expected[i].Name = "ct" + strconv.FormatInt(int64(i), 36)
		expected[i].Host, expected[i].Tok = "", ""
		if expected[i].Kind == "reverse" {
			expected[i].Ports = ""
		}
	}
	if !reflect.DeepEqual(peer.Cases, expected) {
		t.Fatalf("compaction changed peer transport settings: %+v", peer.Cases)
	}
}

func TestConnectionTestICMPCompactionStillBuildsAValidPeer(t *testing.T) {
	for _, carrier := range []string{"xdi", "icmp-request", "icmp-reply"} {
		link := ConnTestLink{Host: "192.0.2.1", Cases: []ShareLink{{Kind: "direct", From: "iran", Tr: carrier, Encap: "gre", Port: "24567", LocalIP: "10.123.231.1/30", PeerIP: "10.123.231.2"}}}
		body, err := json.Marshal(ctCompactLink(link))
		raw := body
		if err != nil {
			t.Fatal(err)
		}
		var out ConnTestLink
		if err = json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		c := out.Cases[0]
		if c.Port != "1" || !validPort(c.Port) {
			t.Fatalf("invalid ignored ICMP port: %q", c.Port)
		}
		c.Tok = "test-shared-secret"
		form := MirrorForPeer(c)
		_, err = form.ToNewDirectTunnel().spec()
		if err != nil {
			t.Fatalf("%s compacted peer: %v", carrier, err)
		}
	}
}

func TestConnectionTestCompressionRemainsReadableByOlderPeers(t *testing.T) {
	wire := ctConfig(ConnTestLink{Mode: "quick", Cases: []ShareLink{{Kind: "direct", From: "iran", Tr: "udp", Port: "53"}}})
	compressed, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil {
		t.Fatal(err)
	}
	r, err := stdgzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ConnTestLink
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Mode != "quick" || len(decoded.Cases) != 1 || decoded.Cases[0].Tr != "udp" {
		t.Fatal("standard gzip reader lost the peer settings")
	}
}

func TestAdditionalDirectSettingsRemainAuthenticatedBoundedAndPaired(t *testing.T) {
	port := ctPickPort(map[int]bool{}, true)
	if port == 0 {
		t.Fatal("no free coordinator port")
	}
	coord, err := startCTCoordinator(port, "shared-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	defer coord.close()
	link := ConnTestLink{Host: "127.0.0.1", Tok: coord.tok, Coord: port, Mode: ConnTestQuick}
	for i, tr := range []string{"udp", "dns", "icmp-request", "icmp-reply"} {
		link.Cases = append(link.Cases, ShareLink{Kind: "direct", From: "iran", Tr: tr, Encap: "gre", Port: strconv.Itoa(25000 + i), Host: link.Host, Tok: ctCaseToken(link.Tok, "direct", tr), LocalIP: "10.123.232.1/30", PeerIP: "10.123.232.2", MTU: 1280, DNSRotate: 6, DNSRate: 5000})
	}
	coord.setTestConfig(link)
	if got := coord.answer("additional wrong-secret", link.Host); got != "" {
		t.Fatal("extra settings ignored authentication")
	}
	main := coord.answer("config "+link.Tok, link.Host)
	extra := coord.answer("additional "+link.Tok, link.Host)
	if len(main) > 1200 || len(extra) > 1200 || !strings.HasPrefix(extra, "additional ") {
		t.Fatal("settings exceeded a single packet")
	}
	decoded, err := unGzipB64(strings.TrimPrefix(extra, "additional "))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(decoded, []byte(link.Cases[1].Tok)) {
		t.Fatal("extra settings carry a tunnel credential")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := fetchConnTestLink(ctx, connTestAddr{Host: link.Host, Coord: port, Tok: link.Tok})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Cases) != len(link.Cases) || out.Mode != link.Mode {
		t.Fatal("additional carriers missing from the full test")
	}
	original := map[string]ShareLink{}
	for _, c := range link.Cases {
		original[c.Tr] = c
	}
	seen := map[string]bool{}
	for _, c := range out.Cases {
		want, ok := original[c.Tr]
		if !ok || seen[c.Name] || c.Tok != want.Tok || c.LocalIP != want.LocalIP || c.Host != want.Host || c.MTU != want.MTU || c.DNSRotate != want.DNSRotate || c.DNSRate != want.DNSRate {
			t.Fatalf("damaged or colliding paired case: %s", c.Tr)
		}
		seen[c.Name] = true
	}
	// Original JSON readers retain all previously supported cases and ignore
	// the optional count; they never try to launch unsupported new carriers.
	raw, _ := unGzipB64(strings.TrimPrefix(main, "config "))
	var legacy ConnTestLink
	if err = json.Unmarshal(raw, &legacy); err != nil || len(legacy.Cases) != 1 || legacy.Cases[0].Tr != "udp" {
		t.Fatal("legacy suite lost an existing protocol")
	}
}
