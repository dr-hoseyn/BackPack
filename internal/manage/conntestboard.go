package manage

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The Connection Test's table, drawn while the test runs.
//
// Each tunnel is one row, and a row changes as its echoes come back: STATUS
// reads TESTING, ECHOES counts the echoes sent so far, and the bar fills with
// them — progress = sent / total, from the test itself, not an animation. Only
// rows that changed are rewritten, in place, so the screen does not flicker
// with seventeen tunnels reporting at once. When the test ends the table is
// set out once more as the verdict: the rows grouped OK, UNSTABLE, DOWN, each
// bar filled by the echoes that came back, and under it the tunnels that
// carried everything, with their round trip and speed.

// ctBarSegments is how many segments a bar has.
const ctBarSegments = 8

// ctTransportNames are the compact names the tables and menus use. Keep
// versions, security layers and optional obfuscation in menu descriptions.
var ctTransportNames = map[string]string{
	"xhttp-reality": "XHTTP REALITY", "trusttunnel": "TrustTunnel", "webtransport": "WebTransport", "tailcat": "Tailcat",
	"awg": "AmneziaWG",
	"tcp": "TCP", "tcpmux": "TCP MUX", "stealth": "Stealth", "pck": "PCK",
	"ws": "WS", "wss": "WSS", "wsmux": "WS MUX", "wssmux": "WSS MUX",
	"kcp": "KCP FEC", "quic": "QUIC", "udp": "UDP", "xdi": "xDi", "sni": "SNI",
	"hysteria2": "Hy2", "anytls": "AnyTLS", "shadowtls": "ShadowTLS", "tuic": "TUIC",
	"masque": "MASQUE H3", "masque-h2": "MASQUE H2", "short-https": "Short HTTPS", "naive": "Naive H2", "xhttp": "XHTTP", "reality": "REALITY",
	ctSpoofToKharej: "Iran→Kharej", ctSpoofToIran: "Kharej→Iran",
}

// Keep every table aligned with the longest protocol label. fmt's string
// widths are minimums, so the old 16/18-column fields let longer names push
// status, RTT and speed into neighbouring columns. Labels use single-cell
// runes; count runes as fmt does, including the spoofing direction arrows.
var ctNameWidth = func() int {
	width := 18
	for _, name := range ctTransportNames {
		width = max(width, utf8.RuneCountInString(name))
	}
	return width
}()

func ctName(tr string) string {
	if n, ok := ctTransportNames[tr]; ok {
		return n
	}
	return strings.ToUpper(tr)
}

func ctKind(kind string) string {
	if kind == "" {
		return ""
	}
	return strings.ToUpper(kind[:1]) + kind[1:]
}

// ctBar is a bar filled to share (0–1), with the percentage beside it.
func ctBar(share float64) string {
	share = math.Max(0, math.Min(1, share))
	filled := int(math.Round(share * ctBarSegments))
	return fmt.Sprintf("%s%s %3d%%", strings.Repeat("▰", filled),
		strings.Repeat("▱", ctBarSegments-filled), int(math.Round(share*100)))
}

func ctEmoji(status string) string {
	switch status {
	case ctOK:
		return "🟢"
	case ctDown:
		return "🔴"
	case ctSkipped:
		return "⚪"
	default: // testing, unstable
		return "🟡"
	}
}

// Every emoji here takes two columns; the rest of the RESULT column is spaces.
const ctRowFormat = "%s       %-9s %-*s %-8s %-6s %6s %s"

var ctHeader = fmt.Sprintf("%-8s %-9s %-*s %-8s %-6s %6s %-13s", "RESULT", "KIND", ctNameWidth, "NAME", "STATUS", "ECHOES", "LOSS", "PROGRESS")

// ctRow is one tunnel's line. While it runs, ECHOES and the bar are the echoes
// sent; once it has a verdict, the echoes that came back.
func ctRow(r ConnTestResult) string {
	total := r.Total
	if total <= 0 {
		total = connTestSoak
	}
	done := r.OK
	if r.Status == ctTesting {
		done = r.Tried
	}
	return fmt.Sprintf(ctRowFormat, ctEmoji(r.Status), ctKind(r.Kind), ctNameWidth, ctResultName(r),
		ctStatusLabel(r), fmt.Sprintf("%d/%d", done, total), ctLossLabel(r), ctBar(float64(done)/float64(total)))
}

// This is failed application-level echo attempts, the same sample used by
// the recommendations, not raw network packet loss: reliable transports can
// recover packets below the echo layer. Unsent attempts and pending replies
// must not be presented as loss.
func ctLossLabel(r ConnTestResult) string {
	if r.Status == ctTesting || r.Status == ctSkipped || r.Tried <= 0 {
		return "-"
	}
	lost := r.Tried - max(0, min(r.OK, r.Tried))
	return fmt.Sprintf("%.1f%%", float64(lost)*100/float64(r.Tried))
}

func ctStatusLabel(r ConnTestResult) string {
	if r.Phase == "speed" {
		return "SPEED"
	}
	if r.Phase == "speed-queued" {
		return "QUEUED"
	}
	return strings.ToUpper(r.Status)
}

func ctRule() string { return strings.Repeat("─", utf8.RuneCountInString(ctHeader)) }

// A short overview keeps skipped prerequisites distinct from failed tests.
// Reuse it above both tables so live progress and the final verdict agree.
func ctSummary(rows []ConnTestResult) string {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Status]++
	}
	parts := []string{}
	for _, group := range []struct{ status, label string }{
		{ctScreened, "screened"}, {ctOK, "steady"}, {ctUnstable, "unstable"}, {ctDown, "down"},
		{ctTesting, "testing"}, {ctSkipped, "skipped"},
	} {
		if n := counts[group.status]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, group.label))
		}
	}
	if len(parts) == 0 {
		return "Summary: no results yet"
	}
	return "Summary: " + strings.Join(parts, " / ")
}

// Diagnostics get their own indented lines instead of wrapping underneath
// the protocol name at the left edge of the terminal. Do not split a token:
// paths and hostnames must remain copyable.
func ctWriteNote(out io.Writer, text string) {
	const indent = "      "
	ctWriteWrapped(out, indent, indent, text)
}

func ctWriteWrapped(out io.Writer, prefix, continuation, text string) {
	line, hasWords := prefix, false
	for _, word := range strings.Fields(text) {
		if hasWords && utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > utf8.RuneCountInString(ctHeader) {
			fmt.Fprintln(out, line)
			line, hasWords = continuation, false
		}
		if hasWords {
			line += " "
		}
		line += word
		hasWords = true
	}
	if hasWords {
		fmt.Fprintln(out, line)
	}
}

// ConnTestTable is the verdict as text: the grouped table and the tunnels
// that carried everything.
func ConnTestTable(results []ConnTestResult) string {
	var b strings.Builder
	if len(results) > 0 && results[0].Mode == ConnTestQuick {
		b.WriteString("QUICK TEST: 20-second observation; short speed samples for every healthy tunnel.\nSCREENED = traffic passed, speed not measured; excluded from automatic setup.\n\n")
	}
	b.WriteString(ctSummary(results) + "\n\n")
	b.WriteString(ctHeader + "\n")
	for _, group := range ctGroups(results) {
		b.WriteString("\n")
		for _, r := range group {
			b.WriteString(ctRow(r) + "\n")
		}
	}
	b.WriteString("\n" + ctRule() + "\n\nWorks Steadily:\n\n")
	steady := 0
	for _, kind := range []string{"reverse", "direct", "awg", "spoof"} {
		var rows []ConnTestResult
		for _, r := range results {
			if r.Status == ctOK && (r.Kind == kind || kind == "reverse" && r.Kind == "") {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			continue
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Mbps > rows[j].Mbps })
		fmt.Fprintf(&b, "  %s\n", ctKind(kind))
		fmt.Fprintf(&b, "    %-*s %8s %12s %12s %6s\n", ctNameWidth, "Transport", "RTT", "Down Mbps", "Up Mbps", "Loss")
		for _, r := range rows {
			steady++
			rtt, down, up := "-", "-", "-"
			if r.RTTms > 0 {
				rtt = fmt.Sprintf("%dms", r.RTTms)
			} else if r.OK > 0 && kind != "spoof" {
				rtt = "<1ms"
			}
			if r.Mbps > 0 {
				down = fmt.Sprintf("%.1f", r.Mbps)
			}
			if r.UploadMbps > 0 {
				up = fmt.Sprintf("%.1f", r.UploadMbps)
			}
			fmt.Fprintf(&b, "    %-*s %8s %12s %12s %6s\n", ctNameWidth, ctResultName(r), rtt, down, up, ctLossLabel(r))
		}
		b.WriteByte('\n')
	}
	if steady > 0 {
		b.WriteString("  Speed uses 4 parallel TCP streams per direction; '-' = not measured.\n")
	}
	b.WriteString("  Loss = failed echo/probe attempts / attempts made.\n" +
		"  '-' = pending or not measured. Retransmissions can hide network packet loss.\n")
	if steady == 0 {
		b.WriteString("Nothing carried traffic steadily between these two servers. See the\n" +
			"statuses and notes for setup errors, interruption or path failures.\n")
	}
	// Why, where the table alone cannot say: a spoofing check that failed, and
	// anything that was not run.
	notes := false
	for _, r := range results {
		if r.Detail != "" && r.Status != ctOK {
			if !notes {
				b.WriteString("\nNotes:\n")
				notes = true
			}
			fmt.Fprintf(&b, "\n  %s %s %s\n", ctEmoji(r.Status), ctKind(r.Kind), ctResultName(r))
			ctWriteNote(&b, r.Detail)
		}
	}
	ctSNINotes(&b, results)
	ctHTTPSNotes(&b, results)
	return b.String()
}

func ctHTTPSNotes(out io.Writer, results []ConnTestResult) {
	shown := false
	for _, r := range results {
		if r.Kind != "reverse" || !managedTransport(r.Transport) || r.Status == ctSkipped || r.TestPort < 1 || r.TestPort > 65535 {
			continue
		}
		if !shown {
			fmt.Fprint(out, "\nHTTPS path checks (separate connections):\n\n")
			shown = true
		}
		fmt.Fprintf(out, "  %-*s :%d\n", ctNameWidth, ctName(r.Transport), r.TestPort)
		ctWriteNote(out, ctHTTPSProbeLabel(r.HTTPSProbe))
	}
	if shown {
		fmt.Fprintln(out, "  Tunnel status above requires real authenticated, sustained traffic.")
		fmt.Fprintln(out, "  Temporary ports/private test certificates differ from a real HTTPS site.")
		fmt.Fprintln(out, "  Changing SNI alone cannot fix a route that drops established flows.")
	}
}

// ctGroups orders the verdict as the table shows it: what worked, fastest
// first; what dropped, most echoes first; then what never came up, reverse and
// direct apart; then what was not run.
func ctGroups(results []ConnTestResult) [][]ConnTestResult {
	var ok, screened, unstable, downReverse, downDirect, downSpoof, skipped []ConnTestResult
	for _, r := range results {
		switch r.Status {
		case ctOK:
			ok = append(ok, r)
		case ctScreened:
			screened = append(screened, r)
		case ctDown:
			switch r.Kind {
			case "direct":
				downDirect = append(downDirect, r)
			case "spoof":
				downSpoof = append(downSpoof, r)
			default:
				downReverse = append(downReverse, r)
			}
		case ctSkipped:
			skipped = append(skipped, r)
		default:
			unstable = append(unstable, r)
		}
	}
	sort.SliceStable(ok, func(i, j int) bool { return ok[i].Mbps > ok[j].Mbps })
	sort.SliceStable(unstable, func(i, j int) bool { return unstable[i].OK > unstable[j].OK })
	var out [][]ConnTestResult
	for _, g := range [][]ConnTestResult{ok, screened, unstable, downReverse, downDirect, downSpoof, skipped} {
		if len(g) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// ctBoard draws the live table and keeps it current.
type ctBoard struct {
	out   io.Writer
	title string
	tty   bool

	mu            sync.Mutex
	rows          []ConnTestResult
	shown         []string // what each line of the drawn region reads now
	stop          chan struct{}
	done          chan struct{}
	size          func() (int, int)
	width, height int
	reset         bool // a zero-sized/minimized terminal invalidated cursor positions
}

// newCTBoard starts a board under title. On a terminal it redraws changed
// rows in place a few times a second; anywhere else it prints only the
// verdict.
func newCTBoard(out io.Writer, title string) *ctBoard {
	b := &ctBoard{out: out, title: title, tty: ctIsTerminal(out),
		stop: make(chan struct{}), done: make(chan struct{})}
	go b.loop()
	return b
}

func ctIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	width, height := ctTerminalSize(w)
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && width > 0 && height > 0
}

// set records one tunnel's state.
func (b *ctBoard) set(i int, r ConnTestResult) {
	b.mu.Lock()
	for len(b.rows) <= i {
		b.rows = append(b.rows, ConnTestResult{})
	}
	b.rows[i] = r
	b.mu.Unlock()
}

// setAll replaces every row, for the kharej, which is sent them together.
func (b *ctBoard) setAll(rows []ConnTestResult) {
	b.mu.Lock()
	b.rows = append([]ConnTestResult(nil), rows...)
	b.mu.Unlock()
}

func (b *ctBoard) loop() {
	defer close(b.done)
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			if b.tty {
				b.draw()
			}
		}
	}
}

// lines is the live region: title, rule, header, one line per tunnel, rule.
func (b *ctBoard) lines(width, height int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.rows) == 0 {
		return nil
	}
	out := []string{"CONNECTION TEST — " + b.title, ctRule(), ctSummary(b.rows), ctHeader, ""}
	compact := width-1 < utf8.RuneCountInString(ctHeader)
	if compact {
		out[2] = strings.NewReplacer("steady", "OK", "skipped", "skip").Replace(out[2])
		out[3] = fmt.Sprintf("   K %-*s %-8s %6s", ctNameWidth, "NAME", "STATUS", "LOSS")
	}
	rows := b.rows
	// Keep the whole live region inside the viewport, including the cursor
	// below it. Cursor-up cannot reach a row that has scrolled off the screen.
	limit := len(rows)
	if height > 0 && len(rows)+7 >= height {
		limit = max(0, height-9)
	}
	if len(rows) > limit {
		rows = append([]ConnTestResult(nil), rows...)
		sort.SliceStable(rows, func(i, j int) bool {
			priority := func(r ConnTestResult) int {
				if r.Phase == "speed" {
					return 0
				}
				if r.Phase == "speed-queued" {
					return 1
				}
				if r.Status == ctTesting {
					return 2
				}
				return 3
			}
			return priority(rows[i]) < priority(rows[j])
		})
		rows = rows[:limit]
	}
	for _, r := range rows {
		if compact {
			kind := ctKind(r.Kind)
			if kind == "" {
				kind = "Reverse"
			}
			out = append(out, fmt.Sprintf("%s %s %-*s %-8s %6s", ctEmoji(r.Status), kind[:1], ctNameWidth, ctResultName(r), ctStatusLabel(r), ctLossLabel(r)))
		} else {
			out = append(out, ctRow(r))
		}
	}
	if len(rows) < len(b.rows) {
		out = append(out, fmt.Sprintf("Showing %d/%d; active tests first. Full results at finish.", len(rows), len(b.rows)))
	}
	hint := ""
	if compact {
		hint = "R reverse / D direct / S spoof"
	}
	out = append(out, hint, ctRule())
	if height > 0 && len(out) >= height {
		out = out[:max(0, height-1)]
	}
	for i := range out {
		out[i] = ctClipLine(out[i], width-1)
	}
	return out
}

// Live lines contain ASCII, the progress glyphs and the four status emoji.
// Reserve the final column so neither wrapping nor delayed autowrap can move
// the cursor down an extra row during an in-place update.
func ctClipLine(line string, columns int) string {
	if columns <= 0 {
		return ""
	}
	used := 0
	for i, r := range line {
		width := 1
		switch r {
		case '🟢', '🟡', '🔴', '⚪':
			width = 2
		}
		if used+width > columns {
			return line[:i]
		}
		used += width
	}
	return line
}

// draw rewrites the lines that changed since the last draw, and nothing else.
func (b *ctBoard) draw() {
	width, height := ctTerminalSize(b.out)
	if b.size != nil {
		width, height = b.size()
	}
	if width <= 0 || height <= 0 {
		b.reset = true
		return
	}
	lines := b.lines(width, height)
	var w strings.Builder
	if b.reset || b.width != 0 && (b.width != width || b.height != height) {
		// A resize may already have pushed old rows into scrollback. Start at
		// a known viewport origin rather than addressing those stale positions.
		w.WriteString("\033[H\033[2J")
		b.shown = nil
	}
	b.reset = false
	b.width, b.height = width, height
	if len(b.shown) != len(lines) {
		// First draw, or the row count changed: the region is written out.
		if n := len(b.shown); n > 0 {
			fmt.Fprintf(&w, "\033[%dF\033[J", n)
		}
		for _, l := range lines {
			w.WriteString(ctThemeLine(l) + "\n")
		}
	} else {
		// The cursor sits under the region; each changed line is reached from
		// there, rewritten, and left.
		for i, l := range lines {
			if l == b.shown[i] {
				continue
			}
			up := len(lines) - i
			fmt.Fprintf(&w, "\033[%dF\033[2K%s\033[%dE", up, ctThemeLine(l), up)
		}
	}
	b.shown = lines
	if w.Len() > 0 {
		_, _ = io.WriteString(b.out, w.String())
	}
}

// finish stops the live drawing and sets the verdict out in its place, and
// the recommendation under it.
func (b *ctBoard) finish(results []ConnTestResult, best ConnTestBest) {
	close(b.stop)
	<-b.done
	var w strings.Builder
	if b.tty && len(b.shown) > 0 {
		width, height := ctTerminalSize(b.out)
		if b.size != nil {
			width, height = b.size()
		}
		if b.reset || width != b.width || height != b.height {
			w.WriteString("\033[H\033[2J")
		} else {
			fmt.Fprintf(&w, "\033[%dF\033[J", len(b.shown))
		}
	}
	text := "CONNECTION TEST — " + b.title + "\n" + ctRule() + "\n\n" +
		ConnTestTable(results) + "\n" + ConnTestBestTable(best)
	if b.tty {
		text = ctThemeText(text)
	}
	w.WriteString(text)
	_, _ = io.WriteString(b.out, w.String())
}

// abandon stops the live drawing and leaves what is on the screen.
func (b *ctBoard) abandon() {
	close(b.stop)
	<-b.done
}
