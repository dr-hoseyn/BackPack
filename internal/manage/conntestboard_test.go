package manage

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTheBarIsFilledByTheRealShare(t *testing.T) {
	for _, c := range []struct {
		share float64
		want  string
	}{
		{0, "▱▱▱▱▱▱▱▱   0%"},
		{30.0 / 60, "▰▰▰▰▱▱▱▱  50%"},
		{22.0 / 60, "▰▰▰▱▱▱▱▱  37%"},
		{45.0 / 60, "▰▰▰▰▰▰▱▱  75%"},
		{1, "▰▰▰▰▰▰▰▰ 100%"},
	} {
		if got := ctBar(c.share); got != c.want {
			t.Errorf("share %.2f: %q, want %q", c.share, got, c.want)
		}
	}
}

// While a tunnel runs, its row counts the echoes sent — progress; once it has
// a verdict, the echoes that came back.
func TestARowShowsProgressWhileTestingAndTheVerdictAfter(t *testing.T) {
	running := ConnTestResult{Kind: "reverse", Transport: "stealth", Status: ctTesting, Tried: 30, OK: 20, Total: 60}
	if row := ctRow(running); !strings.Contains(row, "TESTING") || !strings.Contains(row, "30/60") ||
		!strings.Contains(row, "▰▰▰▰▱▱▱▱  50%") || !strings.HasPrefix(row, "🟡") {
		t.Errorf("running row: %q", row)
	}
	done := ConnTestResult{Kind: "reverse", Transport: "stealth", Status: ctUnstable, Tried: 60, OK: 52, Total: 60}
	if row := ctRow(done); !strings.Contains(row, "UNSTABLE") || !strings.Contains(row, "52/60") ||
		!strings.Contains(row, "87%") || !strings.Contains(row, "Stealth") {
		t.Errorf("unstable row: %q", row)
	}
	down := ConnTestResult{Kind: "direct", Transport: "sni", Status: ctDown, Total: 60}
	if row := ctRow(down); !strings.HasPrefix(row, "🔴") || !strings.Contains(row, "0/60") ||
		!strings.Contains(row, "▱▱▱▱▱▱▱▱   0%") || !strings.Contains(row, "Direct") {
		t.Errorf("down row: %q", row)
	}
	ok := ConnTestResult{Kind: "direct", Transport: "xdi", Status: ctOK, Tried: 60, OK: 60, Total: 60}
	if row := ctRow(ok); !strings.HasPrefix(row, "🟢") || !strings.Contains(row, "▰▰▰▰▰▰▰▰ 100%") || !strings.Contains(row, "xDi") {
		t.Errorf("ok row: %q", row)
	}
}

func TestTheVerdictIsGroupedAndListsWhatWorks(t *testing.T) {
	table := ConnTestTable([]ConnTestResult{
		{Kind: "reverse", Transport: "kcp", Status: ctDown, Total: 60},
		{Kind: "direct", Transport: "udp", Status: ctOK, OK: 60, Tried: 60, Total: 60, RTTms: 73, Mbps: 28},
		{Kind: "reverse", Transport: "tcp", Status: ctUnstable, OK: 29, Tried: 60, Total: 60},
		{Kind: "direct", Transport: "pck", Status: ctOK, OK: 60, Tried: 60, Total: 60, RTTms: 69, Mbps: 29.9},
		{Kind: "direct", Transport: "quic", Status: ctDown, Total: 60},
	})
	order := []string{"Direct    PCK", "Direct    UDP", "Reverse   TCP", "Reverse   KCP FEC", "Direct    QUIC", "Works Steadily:",
		"  Direct\n", "    PCK", "69ms", "29.9", "    UDP", "73ms", "28.0"}
	at := 0
	for _, want := range order {
		i := strings.Index(table[at:], want)
		if i < 0 {
			t.Fatalf("%q missing or out of order in\n%s", want, table)
		}
		at += i + len(want)
	}
}

func TestConnTestTableExplainsLocalFailuresWithoutClaimingFiltering(t *testing.T) {
	table := ConnTestTable([]ConnTestResult{
		{Kind: "reverse", Transport: "naive", Status: ctSkipped, Detail: "helper unavailable"},
		{Kind: "reverse", Transport: "xhttp", Status: ctDown, Detail: "test stopped"},
		{Kind: "reverse", Transport: "reality", Status: ctUnstable, Detail: "bulk transfer failed"},
	})
	for _, want := range []string{"Naive / HTTP2", "XHTTP / TLS", "REALITY / Vision", "helper unavailable", "test stopped", "bulk transfer failed"} {
		if !strings.Contains(table, want) {
			t.Errorf("table is missing %q", want)
		}
	}
	if strings.Contains(table, "filtered for every transport") {
		t.Fatal("local failures were reported as route filtering")
	}
}

// Only the rows that changed are written again.
func TestTheLiveTableRewritesOnlyWhatChanged(t *testing.T) {
	var out bytes.Buffer
	b := &ctBoard{out: &out, title: "1.2.3.4", tty: true, size: func() (int, int) { return 100, 40 }}
	b.set(0, ConnTestResult{Kind: "reverse", Transport: "tcp", Status: ctTesting, Total: 60})
	b.set(1, ConnTestResult{Kind: "direct", Transport: "pck", Status: ctTesting, Total: 60})
	b.draw()
	if !strings.Contains(out.String(), "CONNECTION TEST — 1.2.3.4") {
		t.Fatalf("first draw: %q", out.String())
	}
	out.Reset()
	b.draw()
	if out.Len() != 0 {
		t.Errorf("nothing changed, yet %q was written", out.String())
	}
	b.set(1, ConnTestResult{Kind: "direct", Transport: "pck", Status: ctTesting, Tried: 1, OK: 1, Total: 60})
	b.draw()
	if n := strings.Count(out.String(), "\033[2K"); n != 1 || !strings.Contains(out.String(), "1/60") {
		t.Errorf("one row changed; %d lines rewritten: %q", n, out.String())
	}
}

func TestConnTestManagedNamesKeepColumnsAligned(t *testing.T) {
	rows := []ConnTestResult{}
	for tr := range ctTransportNames {
		rows = append(rows, ConnTestResult{Kind: "reverse", Transport: tr, Status: ctOK, OK: 60, Tried: 60, RTTms: 26, Mbps: 60.3, UploadMbps: 41.2})
	}
	table := ConnTestTable(rows)
	steady := strings.Split(table, "Works Steadily:")[1]
	column := -1
	for _, line := range strings.Split(steady, "\n") {
		if !strings.Contains(line, "26ms") {
			continue
		}
		at := utf8.RuneCountInString(line[:strings.Index(line, "26ms")])
		if column < 0 {
			column = at
		}
		if at != column {
			t.Fatalf("RTT column moved: %q", line)
		}
		if !strings.Contains(line, "  26ms") {
			t.Fatalf("name touches RTT: %q", line)
		}
	}
	for _, r := range rows {
		line := strings.Replace(ctRow(r), "🟢", "  ", 1)
		if utf8.RuneCountInString(line[:strings.Index(line, "OK")]) != strings.Index(ctHeader, "STATUS") {
			t.Fatalf("header drifted: %q / %q", ctHeader, line)
		}
	}
	measured := -1
	for tr := range ctTransportNames {
		table := ConnTestBestTable(ConnTestBest{Transport: "reverse " + tr, Mbps: 60.3})
		for _, line := range strings.Split(table, "\n") {
			if !strings.HasPrefix(line, "Transport") {
				continue
			}
			at := utf8.RuneCountInString(line[:strings.Index(line, "60.3 Mbps")])
			if measured < 0 {
				measured = at
			}
			if measured != at {
				t.Fatalf("best settings column moved: %q", line)
			}
		}
	}
}

func TestConnectionTestRowsReportLossFromAttemptsActuallyMade(t *testing.T) {
	end := strings.Index(ctHeader, "LOSS") + len("LOSS")
	for _, c := range []struct {
		name, status string
		tried, ok    int
		want         string
	}{
		{"steady", ctOK, 60, 60, "0.0%"},
		{"partial delivery", ctUnstable, 60, 52, "13.3%"},
		{"interrupted after ten attempts", ctUnstable, 10, 8, "20.0%"},
		{"all attempts failed", ctDown, 60, 0, "100.0%"},
		{"could not connect", ctDown, 0, 0, "-"},
		{"reply pending", ctTesting, 30, 29, "-"},
		{"skipped", ctSkipped, 60, 0, "-"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := ConnTestResult{Kind: "reverse", Transport: "hysteria2", Status: c.status, Tried: c.tried, OK: c.ok, Total: 60}
			table := ConnTestTable([]ConnTestResult{r})
			plain := strings.NewReplacer("🟢", "  ", "🟡", "  ", "🔴", "  ", "⚪", "  ").Replace(table)
			for _, line := range strings.Split(plain, "\n") {
				if !strings.Contains(line, "Reverse   Hysteria2") {
					continue
				}
				if got := strings.TrimSpace(line[end-6 : end]); got != c.want {
					t.Fatalf("loss = %q, want %q in\n%s", got, c.want, table)
				}
				return
			}
			t.Fatal("result row missing")
		})
	}
}

func TestConnectionTestDiagnosticsKeepTheirOwnIndentation(t *testing.T) {
	detail := "no probe from 10.10.10.10 arrived — a private source is often dropped on the way; a public one may pass (Manage → IP Spoofing Tester)"
	table := ConnTestTable([]ConnTestResult{
		{Kind: "spoof", Transport: ctSpoofToKharej, Status: ctDown, Tried: 20, Detail: detail},
		{Kind: "reverse", Transport: "naive", Status: ctSkipped, Detail: "helper unavailable"},
	})
	if !strings.HasPrefix(table, "Summary: 1 down / 1 skipped\n") {
		t.Fatal(table)
	}
	notes := strings.Split(table, "Notes:\n")[1]
	var wrapped []string
	for _, line := range strings.Split(notes, "\n") {
		if utf8.RuneCountInString(line) > utf8.RuneCountInString(ctHeader) {
			t.Fatalf("diagnostic exceeded the table: %q", line)
		}
		if strings.HasPrefix(line, "      ") {
			wrapped = append(wrapped, strings.TrimSpace(line))
		}
	}
	if got := strings.Join(wrapped, " "); got != detail+" helper unavailable" {
		t.Fatalf("diagnostic text changed: %q", got)
	}
}
