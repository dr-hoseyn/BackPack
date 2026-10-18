package manage

import (
	"encoding/json"
	"reflect"
	"testing"
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
		expected[i].Host, expected[i].Tok = "", ""
		if expected[i].Kind == "reverse" {
			expected[i].Ports = ""
		}
	}
	if !reflect.DeepEqual(peer.Cases, expected) {
		t.Fatalf("compaction changed peer transport settings: %+v", peer.Cases)
	}
}
