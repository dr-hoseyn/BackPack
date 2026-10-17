package manage

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func ctPurposeFixture() ([]ConnTestResult, []*connTestCase) {
	rows := []ConnTestResult{
		{Kind: "reverse", Transport: "xhttp", Status: ctOK, OK: 4, Tried: 4, Mbps: 500, UploadMbps: 20, SpeedStable: false},
		{Kind: "direct", Transport: "sni", CaseID: "sni@www.microsoft.com", SNIDomain: "www.microsoft.com", Status: ctOK, OK: 4, Tried: 4, Mbps: 80, UploadMbps: 40, SpeedStable: true},
		{Kind: "reverse", Transport: "quic", Status: ctOK, OK: 4, Tried: 4, Mbps: 120, UploadMbps: 100, SpeedStable: true},
	}
	cases := []*connTestCase{
		{rtts: []time.Duration{60 * time.Millisecond, 100 * time.Millisecond, 60 * time.Millisecond, 100 * time.Millisecond}},
		{rtts: []time.Duration{9 * time.Millisecond, 11 * time.Millisecond, 9 * time.Millisecond, 11 * time.Millisecond}},
		{rtts: []time.Duration{30 * time.Millisecond, 30 * time.Millisecond, 30 * time.Millisecond, 30 * time.Millisecond}},
	}
	return rows, cases
}

func TestConnectionTestPurposeChoicesMatchTheUsersDifferentNeeds(t *testing.T) {
	rows, cases := ctPurposeFixture()
	choices := ctPurposeRecommendations(rows, cases)
	if len(choices) != 3 || choices[0].Purpose != "download" || choices[0].Transport != "reverse xhttp" ||
		choices[1].Purpose != "realtime" || choices[1].Transport != "direct sni" ||
		choices[2].Purpose != "stability" || choices[2].Transport != "reverse quic" {
		t.Fatalf("download, interactive latency and observed stability were collapsed: %+v", choices)
	}
	if choices[1].SNIDomain != rows[1].SNIDomain || choices[1].CaseID != rows[1].CaseID || choices[1].RTTms != 10 || choices[1].JitterMs != 1 || choices[1].UploadMbps != 40 {
		t.Fatalf("the chosen SNI or its measurements were lost: %+v", choices[1])
	}
	table := ConnTestRecommendationsTable(choices)
	for _, want := range []string{"Download", "Games / calls", "Stability", "www.microsoft.com", "speed varied", "4 echoes", "long-term uptime"} {
		if !strings.Contains(table, want) {
			t.Errorf("missing %q: %s", want, table)
		}
	}
}

func TestConnectionTestPurposeChoicesExcludeFailedAndUnfinishedTraffic(t *testing.T) {
	for _, status := range []string{ctDown, ctUnstable, ctSkipped, ctTesting} {
		rows, cases := ctPurposeFixture()
		for i := range rows {
			rows[i].Status = status
		}
		if got := ctPurposeRecommendations(rows, cases); len(got) != 0 {
			t.Fatalf("recommended %s traffic: %+v", status, got)
		}
	}
	for _, phase := range []string{"speed", "speed-queued"} {
		rows, cases := ctPurposeFixture()
		for i := range rows {
			rows[i].Phase = phase
		}
		if got := ctPurposeRecommendations(rows, cases); len(got) != 0 {
			t.Fatalf("recommended incomplete %s: %+v", phase, got)
		}
	}
	rows, cases := ctPurposeFixture()
	for i := range rows {
		rows[i].Kind = "spoof"
	}
	if got := ctPurposeRecommendations(rows, cases); len(got) != 0 {
		t.Fatalf("recommended capability probes: %+v", got)
	}
}

func TestConnectionTestPurposeChoicesDoNotInventMissingMeasurements(t *testing.T) {
	rows, cases := ctPurposeFixture()
	if got := ctPurposeRecommendations(rows, nil); len(got) != 1 || got[0].Purpose != "download" {
		t.Fatalf("missing RTT samples became zero jitter: %+v", got)
	}
	rows = []ConnTestResult{{Kind: "reverse", Transport: "udp", Status: ctOK, OK: 4, Tried: 4}}
	choices := ctPurposeRecommendations(rows, cases[:1])
	if len(choices) != 2 || choices[0].Purpose != "realtime" || choices[1].Purpose != "stability" || choices[0].SpeedMeasured {
		t.Fatalf("UDP echoes were hidden or their bandwidth invented: %+v", choices)
	}
	for _, invalid := range []float64{math.NaN(), math.Inf(1), -1} {
		rows[0].Mbps = invalid
		if got := ctPurposeRecommendations(rows, cases[:1]); len(got) != 0 {
			t.Fatalf("non-finite or negative result recommended: %+v", got)
		}
	}
}

func TestConnectionTestInteractiveChoiceAccountsForJitterAndUpload(t *testing.T) {
	rows, cases := ctPurposeFixture()
	rows, cases = rows[1:], cases[1:]
	// A low average with very uneven delays must not beat a consistent path.
	cases[0].rtts = []time.Duration{time.Millisecond, 49 * time.Millisecond, time.Millisecond, 49 * time.Millisecond}
	got := ctPurposeRecommendations(rows, cases)
	if got[1].Transport != "reverse quic" {
		t.Fatalf("jitter was ignored: %+v", got)
	}
	// Equal download is broken by verified upload, not by the result order.
	rows[0].Mbps, rows[1].Mbps = 100, 100
	got = ctPurposeRecommendations(rows, cases)
	if got[0].Transport != "reverse quic" {
		t.Fatalf("upload was ignored: %+v", got)
	}
}

func TestConnectionTestSuiteKeepsEachPurposeOnItsOwnIPPath(t *testing.T) {
	rows, cases := ctPurposeFixture()
	v4 := ctPurposeRecommendations(rows[:1], cases[:1])
	v6 := ctPurposeRecommendations(rows[1:], cases[1:])
	paths := []ConnTestPath{
		{Host: "192.0.2.1", Family: "IPv4", State: "done", Best: ConnTestBest{Transport: "reverse xhttp", Mbps: 500, Recommendations: v4}},
		{Host: "2001:db8::1", Family: "IPv6", State: "done", Best: ConnTestBest{Transport: "reverse quic", Mbps: 120, Recommendations: v6}},
		{Host: "192.0.2.99", Family: "IPv4", State: "running", Best: ConnTestBest{Transport: "reverse xhttp", Mbps: 1000, Recommendations: v4}},
	}
	b := ConnTestSuiteBest(paths)
	if b.Transport != "reverse xhttp" || len(b.Recommendations) != 3 || b.Recommendations[0].Host != paths[0].Host ||
		b.Recommendations[1].Host != paths[1].Host || b.Recommendations[2].Host != paths[1].Host {
		t.Fatalf("suite mixed paths or included an unfinished path: %+v", b)
	}
	if b.Recommendations[1].SNIDomain != rows[1].SNIDomain || v4[0].Host != "" || v6[0].Host != "" {
		t.Fatal("suite lost the SNI identity or mutated a per-path verdict")
	}
	var out strings.Builder
	ctSuiteSummary(&out, paths)
	if !strings.Contains(out.String(), "Games / calls: Direct SNI · www.microsoft.com · IPv6 · 2001:db8::1") {
		t.Fatal(out.String())
	}
	// The verdict is carried by the authenticated coordinator; all new fields
	// must survive its JSON transport, and old JSON still decodes unchanged.
	raw, err := json.Marshal(ctVerdict{Best: b})
	if err != nil {
		t.Fatal(err)
	}
	var decoded ctVerdict
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Best.Recommendations) != 3 || decoded.Best.Recommendations[1].Host != paths[1].Host {
		t.Fatalf("verdict lost choices: %s, %v", raw, err)
	}
	var old ConnTestBest
	if err := json.Unmarshal([]byte(`{"tr":"reverse tcp","mb":20}`), &old); err != nil || old.Transport != "reverse tcp" || len(old.Recommendations) != 0 {
		t.Fatalf("old verdict broke: %+v %v", old, err)
	}
}
