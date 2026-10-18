package manage

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A small VT screen, including autowrap, scrolling and cursor clamping. This
// checks the visible result, rather than just counting ANSI escape sequences.
type ctTestScreen struct {
	w, h, x, y int
	rows       [][]rune
}

func (s *ctTestScreen) blank() []rune { return []rune(strings.Repeat(" ", s.w)) }
func (s *ctTestScreen) resize(w, h int) {
	old := s.rows
	s.w, s.h = w, h
	s.rows = nil
	if len(old) > h {
		old = old[len(old)-h:]
	}
	for _, row := range old {
		n := s.blank()
		copy(n, row)
		s.rows = append(s.rows, n)
	}
	for len(s.rows) < h {
		s.rows = append(s.rows, s.blank())
	}
	s.x, s.y = min(s.x, w-1), min(s.y, h-1)
}
func (s *ctTestScreen) down() {
	s.x = 0
	if s.y < s.h-1 {
		s.y++
		return
	}
	s.rows = append(s.rows[1:], s.blank())
}
func (s *ctTestScreen) feed(text string) {
	text = strings.NewReplacer("🟢", "oo", "🟡", "oo", "🔴", "oo", "⚪", "oo").Replace(text)
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			j := i + 2
			for j < len(runes) && (runes[j] < '@' || runes[j] > '~') {
				j++
			}
			if j == len(runes) {
				panic("partial CSI")
			}
			n, _ := strconv.Atoi(string(runes[i+2 : j]))
			switch runes[j] {
			case 'F':
				s.y = max(0, s.y-max(1, n))
				s.x = 0
			case 'E':
				s.y = min(s.h-1, s.y+max(1, n))
				s.x = 0
			case 'H':
				s.x, s.y = 0, 0
			case 'm': // SGR has no effect on screen geometry
			case 'K':
				s.rows[s.y] = s.blank()
			case 'J':
				if n == 2 {
					for row := range s.rows {
						s.rows[row] = s.blank()
					}
				} else {
					for x := s.x; x < s.w; x++ {
						s.rows[s.y][x] = ' '
					}
					for row := s.y + 1; row < s.h; row++ {
						s.rows[row] = s.blank()
					}
				}
			default:
				panic(fmt.Sprintf("unsupported CSI %c", runes[j]))
			}
			i = j
			continue
		}
		switch r {
		case '\n':
			s.down()
		case '\r':
			s.x = 0
		default:
			if s.x == s.w {
				s.down()
			}
			s.rows[s.y][s.x] = r
			s.x++
		}
	}
}
func (s *ctTestScreen) text() string {
	var out []string
	for _, row := range s.rows {
		out = append(out, string(row))
	}
	return strings.Join(out, "\n")
}

func TestConnectionTestLiveRowsDoNotDuplicateOnSmallOrResizedTerminals(t *testing.T) {
	rowRE := regexp.MustCompile(`(?m)(Reverse|Direct|Spoof|R|D|S)\s+(.+?)\s+(?:OK|SPEED|QUEUED|TESTING|DOWN|SKIPPED)\b`)
	for _, dimensions := range [][2]int{{100, 40}, {80, 14}, {50, 16}} {
		t.Run(fmt.Sprint(dimensions), func(t *testing.T) {
			w, h := dimensions[0], dimensions[1]
			var output bytes.Buffer
			b := &ctBoard{out: &output, tty: true, title: "IPv6 · 2001:db8:1234:5678:abcd:ef01:2345:6789",
				size: func() (int, int) { return w, h }}
			screen := &ctTestScreen{}
			screen.resize(w, h)
			screen.y = h - 3 // start near the bottom, following ordinary menu output
			for _, tr := range connTestReverse {
				b.rows = append(b.rows, ConnTestResult{Kind: "reverse", Transport: tr, Status: ctTesting})
			}
			for _, tr := range connTestDirect {
				b.rows = append(b.rows, ConnTestResult{Kind: "direct", Transport: tr, Status: ctTesting})
			}
			draw := func() {
				t.Helper()
				b.draw()
				screen.feed(output.String())
				output.Reset()
				seen := map[string]bool{}
				for _, match := range rowRE.FindAllStringSubmatch(screen.text(), -1) {
					key := match[1] + "/" + strings.TrimSpace(match[2])
					if seen[key] {
						t.Fatalf("duplicate %s on %dx%d screen:\n%s", key, w, h, screen.text())
					}
					seen[key] = true
				}
				if len(seen) == 0 || (h >= len(b.rows)+8 && len(seen) != len(b.rows)) {
					t.Fatalf("missing live rows on %dx%d screen:\n%s", w, h, screen.text())
				}
			}
			draw()
			for i := range b.rows {
				b.rows[i].Phase = "speed"
				draw()
				b.rows[i].Phase, b.rows[i].Status = "", ctOK
				draw()
			}
			w, h = 50, 14
			screen.resize(w, h)
			draw()
			b.rows[0].Phase = "speed"
			draw()
			if !strings.Contains(screen.text(), "SPEED") || !strings.Contains(screen.text(), "Showing") {
				t.Fatalf("small screen hid the active test or truncation notice:\n%s", screen.text())
			}
			// Shrinking below the header height and restoring the same width
			// must repaint everything, even when no live rows fit meanwhile.
			w, h = 50, 1
			screen.resize(w, h)
			b.draw()
			screen.feed(output.String())
			output.Reset()
			w, h = 50, 14
			screen.resize(w, h)
			draw()
			w, h = 0, 0
			b.draw()
			screen.feed("\x1b[H\x1b[2J") // a minimized client lost its viewport
			w, h = 50, 14
			draw()
			if !strings.Contains(screen.text(), "SPEED") {
				t.Fatal("minimized terminal did not recover")
			}
		})
	}
}

func TestConnectionTestLiveTableFitsNamesLossAndProgressInEightyColumns(t *testing.T) {
	var output bytes.Buffer
	b := &ctBoard{out: &output, tty: true, title: "192.0.2.1", size: func() (int, int) { return 80, 24 }}
	b.rows = []ConnTestResult{
		{Kind: "reverse", Transport: "hysteria2", Status: ctOK, Tried: 60, OK: 60, Total: 60},
		{Kind: "reverse", Transport: "shadowtls", Status: ctUnstable, Tried: 60, OK: 52, Total: 60},
		{Kind: "direct", Transport: "masque-h2", Status: ctOK, Tried: 60, OK: 60, Total: 60},
	}
	b.draw()
	screen := &ctTestScreen{}
	screen.resize(80, 24)
	screen.feed(output.String())
	for _, want := range []string{"Hy2", "ShadowTLS", "MASQUE H2", "LOSS", "0.0%", "13.3%", "100%", "87%"} {
		if !strings.Contains(screen.text(), want) {
			t.Fatalf("%q hidden in an 80-column terminal:\n%s", want, screen.text())
		}
	}
}

func TestConnectionTestNarrowTableKeepsFullNamesStatusAndLossVisible(t *testing.T) {
	var output bytes.Buffer
	b := &ctBoard{out: &output, tty: true, title: "192.0.2.1", size: func() (int, int) { return 50, 18 }}
	b.rows = []ConnTestResult{
		{Kind: "reverse", Transport: "hysteria2", Status: ctUnstable, Tried: 60, OK: 52, Total: 60},
		{Kind: "direct", Transport: "hysteria2", Status: ctDown, Tried: 60, Total: 60},
		{Kind: "direct", Transport: "masque-h2", Status: ctOK, Tried: 60, OK: 60, Total: 60},
	}
	b.draw()
	screen := &ctTestScreen{}
	screen.resize(50, 18)
	screen.feed(output.String())
	// Column padding follows the longest supported protocol label. Ignore
	// padding, while retaining line breaks so wrapped rows still fail.
	lines := strings.Split(screen.text(), "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	visible := strings.Join(lines, "\n")
	for _, want := range []string{"R Hy2 UNSTABLE", "D Hy2 DOWN", "D MASQUE H2 OK", "13.3%", "100.0%", "R reverse / D direct / S spoof"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("%q hidden in a narrow terminal:\n%s", want, screen.text())
		}
	}
}
