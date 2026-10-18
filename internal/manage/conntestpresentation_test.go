package manage

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestConnectionTestStartupKeepsSkippedReasonsAndCoverScope(t *testing.T) {
	s := &ConnTestIran{link: ConnTestLink{Host: "203.0.113.10", Preset: PresetTurbo},
		realityTarget: "www.apple.com:443", realityLog: "/tmp/private-test/reality-cover.log",
		cases: []*connTestCase{
			{kind: "reverse", tr: "tcp"}, {kind: "reverse", tr: "reality"},
			{kind: "reverse", tr: "naive", skip: "sing-box helper is not installed"},
		}}
	var out bytes.Buffer
	ctMenuStage(&out, "1", "Preparing test tunnels", "Checking listeners and covers.")
	ctMenuStartup(&out, &ConnTestSuite{tests: []*ConnTestIran{s}})
	for _, want := range []string{"2 ready / 1 skipped", "www.apple.com:443", "two-server test starts after Kharej joins",
		"Apple fallback", "sing-box helper is not installed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing startup information %q:\n%s", want, out.String())
		}
	}
	s.realityTarget = "www.bing.com:443"
	out.Reset()
	ctMenuStartup(&out, &ConnTestSuite{tests: []*ConnTestIran{s}})
	if strings.Contains(out.String(), "Apple fallback") {
		t.Error("Apple warning was shown for another cover")
	}
	s.realityTarget = ""
	s.cases[1].skip = "no compatible REALITY cover"
	out.Reset()
	ctMenuStartup(&out, &ConnTestSuite{tests: []*ConnTestIran{s}})
	if !strings.Contains(out.String(), s.cases[1].skip) || !strings.Contains(out.String(), s.realityLog) {
		t.Error("failed REALITY cover lost its reason or diagnostic path")
	}
}

func TestConnectionTestStartupGroupsSharedWarningsAndExplainsIPv6Skips(t *testing.T) {
	missing := func(tool string) string {
		return tool + " helper is not installed at /usr/local/lib/backpack/helpers/" + tool + "/pinned/tool; reinstall with BP_HELPERS=naive,xray"
	}
	makePath := func(host string, ipv6 bool) *ConnTestIran {
		s := &ConnTestIran{link: ConnTestLink{Host: host, Preset: PresetTurbo}}
		for _, tr := range connTestReverse {
			c := &connTestCase{kind: "reverse", tr: tr}
			switch tr {
			case "naive":
				c.skip = missing("sing-box")
			case "xhttp", "reality":
				c.skip = missing("xray")
			case "pck":
				if ipv6 {
					c.skip = "PCK uses raw IPv4 packets; IPv6 is not supported"
				}
			}
			s.cases = append(s.cases, c)
		}
		for _, tr := range []string{"udp", "quic", "pck", "xdi", "sni"} {
			c := &connTestCase{kind: "direct", tr: tr}
			if ipv6 && tr != "udp" && tr != "quic" {
				c.skip = "this raw carrier uses IPv4 packets; IPv6 is not supported"
			}
			s.cases = append(s.cases, c)
		}
		for _, tr := range []string{ctSpoofToKharej, ctSpoofToIran} {
			c := &connTestCase{kind: "spoof", tr: tr}
			if ipv6 {
				c.skip = "IP spoofing uses IPv4 packets; IPv6 is not supported"
			}
			s.cases = append(s.cases, c)
		}
		return s
	}
	suite := &ConnTestSuite{tests: []*ConnTestIran{makePath("203.0.113.10", false), makePath("2001:db8::10", true)}}
	var out bytes.Buffer
	ctMenuStartup(&out, suite)
	text := out.String()
	t.Log("\n" + text)
	for _, want := range []string{fmt.Sprintf("%d ready / 3 skipped", len(connTestReverse)+4), fmt.Sprintf("%d ready / 9 skipped", len(connTestReverse)-2), "not network failures",
		"[IPv4, IPv6] Naive H2", "XHTTP, REALITY", "These tests use IPv4 packets",
		"Direct PCK", "Direct xDi", "Direct SNI", "IP spoofing", "both servers", "also updates the BackPack binary"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
	for _, once := range []string{"Preset:", "sing-box helper is not installed", "xray helper is not installed", "BP_HELPERS=naive,xray"} {
		if n := strings.Count(text, once); n != 1 {
			t.Errorf("%q occurs %d times:\n%s", once, n, text)
		}
	}
	if strings.Contains(text, "/usr/local/lib/backpack/helpers/") {
		t.Fatal("repeated internal paths clutter the summary")
	}
	// Presentation must not discard the full diagnosis from the test result.
	if got := suite.tests[0].cases[11].skip; got != missing("sing-box") {
		t.Fatalf("skip reason was changed: %q", got)
	}
}

func TestConnectionTestStartupKeepsUnrelatedFailuresAndUnavailablePaths(t *testing.T) {
	why := "could not start here: bind: address already in use"
	suite := &ConnTestSuite{paths: []ConnTestPath{
		{Host: "203.0.113.10", Family: "IPv4"},
		{Host: "2001:db8::10", Family: "IPv6", Error: "cannot bind IPv6 coordinator"},
	}, tests: []*ConnTestIran{
		{link: ConnTestLink{Host: "203.0.113.10", Preset: PresetTurbo}, cases: []*connTestCase{{kind: "reverse", tr: "tcp", skip: why}}}, nil,
	}}
	var out bytes.Buffer
	ctMenuStartup(&out, suite)
	for _, want := range []string{why, "0 ready / 1 skipped", "unavailable", "cannot bind IPv6 coordinator", "2001:db8::10"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "BP_HELPERS") {
		t.Fatal("helper installation was suggested for an unrelated failure")
	}
}
