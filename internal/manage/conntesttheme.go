package manage

import (
	"strings"

	"github.com/backpack/backpack/internal/tui"
)

// Apply Backpack's red/white/gray theme after measuring and clipping plain
// text, so ANSI sequences cannot change column widths or cursor accounting.
func ctThemeLine(line string) string {
	text := strings.TrimSpace(line)
	color := tui.White
	switch {
	case strings.HasPrefix(text, "CONNECTION TEST"), text == "Works Steadily:", text == "BEST SETTINGS", text == "RECOMMENDED BY USE",
		text == "Notes:", text == "Direct SNI domains:", text == "HTTPS path checks:":
		color = tui.Bold + tui.Red
	case text == "Reverse", text == "Direct", text == "Spoof":
		color = tui.Bold + tui.White
	case text == "", strings.Trim(text, "─") == "", strings.HasPrefix(text, "RESULT"),
		strings.HasPrefix(text, "SETTING"), strings.HasPrefix(text, "Transport") && strings.Contains(text, "RTT"),
		strings.HasPrefix(text, "Speed uses"), strings.HasPrefix(text, "Showing"), strings.HasPrefix(text, "⚪"):
		color = tui.Gray
	case strings.HasPrefix(text, "🔴"):
		color = tui.Red
	}
	return tui.Reset + tui.Color(color, line)
}

func ctThemeText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = ctThemeLine(line)
	}
	return strings.Join(lines, "\n")
}
