package manage

import (
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/tui"
)

func ctMenuStage(out io.Writer, number, title, detail string) {
	fmt.Fprintf(out, "%s\n  %s %s\n  %s\n",
		tui.Color(tui.Gray, ctRule()),
		tui.Color(tui.Bold+tui.Red, number+"."), tui.Color(tui.Bold+tui.White, title),
		tui.Color(tui.Gray, detail))
}

// ctMenuStartup prints the selected paths together. Shared preparation warnings
// belong to the suite, rather than being repeated under each IP address.
func ctMenuStartup(out io.Writer, suite *ConnTestSuite) {
	fmt.Fprintln(out)
	width := len("Iran address")
	preset := ""
	for _, s := range suite.tests {
		if s != nil {
			width = max(width, len(s.link.Host))
			if preset == "" {
				preset = s.link.Preset
			}
		}
	}
	for _, p := range suite.paths {
		width = max(width, len(p.Host))
	}
	fmt.Fprintf(out, "  Preset: %s\n\n", tui.Color(tui.White, titleWord(preset)))
	fmt.Fprintf(out, "    %-6s %-*s %s\n", "Path", width, "Iran address", "Tests")
	type skippedGroup struct {
		reason       string
		paths, names []string
	}
	var groups []skippedGroup
	groupIndex := map[string]int{}
	helpersMissing := false
	for i, s := range suite.tests {
		if s == nil {
			if i < len(suite.paths) {
				p := suite.paths[i]
				fmt.Fprintf(out, "  %s %-6s %-*s unavailable\n", checkMark(CheckFail), p.Family, width, p.Host)
				fmt.Fprintln(out, "    "+p.Error)
			}
			continue
		}
		family := ctFamily(s.link.Host)
		ready, skipped := 0, 0
		for _, c := range s.cases {
			if c.skip == "" {
				ready++
				continue
			}
			skipped++
			reason, missing := ctStartupSkipReason(c.skip)
			helpersMissing = helpersMissing || missing
			key, found := groupIndex[reason]
			if !found {
				key = len(groups)
				groupIndex[reason] = key
				groups = append(groups, skippedGroup{reason: reason})
			}
			g := &groups[key]
			if !slices.Contains(g.paths, family) {
				g.paths = append(g.paths, family)
			}
			name := ctName(c.tr)
			if c.kind == "direct" {
				name = "Direct " + name
			}
			if c.kind == "spoof" {
				name = "IP spoofing " + name
			}
			if !slices.Contains(g.names, name) {
				g.names = append(g.names, name)
			}
		}
		level := CheckOK
		if ready == 0 {
			level = CheckFail
		}
		fmt.Fprintf(out, "  %s %-6s %-*s %d ready / %d skipped\n", checkMark(level), family, width, s.link.Host, ready, skipped)
	}
	if len(groups) > 0 {
		fmt.Fprintln(out, "\n  Skipped tests (not network failures):")
		for _, g := range groups {
			ctStartupNames(out, strings.Join(g.paths, ", "), g.names)
			fmt.Fprintln(out, "      "+g.reason)
		}
	}
	if helpersMissing {
		fmt.Fprintln(out, "\n  To enable the optional HTTPS tests, install helpers on both servers,")
		fmt.Fprintln(out, "  then start a new test. This installer also updates the BackPack binary:")
		fmt.Fprintf(out, "    curl -fsSL \\\n      https://raw.githubusercontent.com/%s/%s/main/install.sh |\n      sudo env BP_HELPERS=naive,xray bash\n", app.RepoOwner, app.RepoName)
	}
	// Scope cover and certificate notes to their path; distinct local probe
	// failures must retain their own diagnostic location.
	notes := map[string]bool{}
	note := func(text string) {
		if !notes[text] {
			fmt.Fprintln(out, tui.Color(tui.Gray, "    "+text))
			notes[text] = true
		}
	}
	for _, s := range suite.tests {
		if s == nil {
			continue
		}
		for _, c := range s.cases {
			if c.skip == "" && (c.tr == "naive" || c.tr == "xhttp") {
				note("HTTPS tests: temporary ports and private certificates.")
				note("XHTTP uses a synthetic SNI; Naive connects by IP. These are not real-site/CDN tests.")
				break
			}
		}
		if s.realityTarget != "" {
			note("REALITY cover: " + s.realityTarget + " · verified on " + ctFamily(s.link.Host))
			note("Cover check passed here; the two-server test starts after Kharej joins.")
			host, _, _ := net.SplitHostPort(s.realityTarget)
			if host == "www.apple.com" {
				note("Apple fallback selected; Xray cautions against Apple/iCloud targets for permanent tunnels.")
			}
			note("Test listeners use temporary ports. Prefer port 443 for a permanent REALITY tunnel.")
		}
		if s.realityLog != "" {
			for _, c := range s.cases {
				if c.tr == "reality" && c.skip != "" {
					note("REALITY probe details (" + ctFamily(s.link.Host) + "): " + s.realityLog)
					break
				}
			}
		}
	}
	for _, s := range suite.tests {
		if s == nil {
			continue
		}
		var rows []ConnTestResult
		for _, c := range s.cases {
			rows = append(rows, c.initialResult())
		}
		ctSNINotes(out, rows)
		break
	}
}

// Keep the full skip diagnostics on each test case and in its final result.
// Preparation groups only the known, repeated prerequisite reasons.
func ctStartupSkipReason(reason string) (string, bool) {
	for _, tool := range []string{"sing-box", "xray", "naive"} {
		if strings.HasPrefix(reason, tool+" helper is not installed") {
			return tool + " helper is not installed or is not executable.", true
		}
	}
	if strings.Contains(reason, "IPv4 packets") && strings.Contains(reason, "IPv6 is not supported") {
		return "These tests use IPv4 packets; IPv6 is not supported.", false
	}
	return reason, false
}

// Wrap the list between case names so combined IPv6 exclusions stay readable.
func ctStartupNames(out io.Writer, scope string, names []string) {
	prefix := "    [" + scope + "] "
	line := prefix
	indent := strings.Repeat(" ", utf8.RuneCountInString(prefix))
	for i, name := range names {
		if i == 0 {
			line += name
			continue
		}
		if utf8.RuneCountInString(line+", "+name) > 78 {
			fmt.Fprintln(out, line+",")
			line = indent + name
		} else {
			line += ", " + name
		}
	}
	fmt.Fprintln(out, line)
}
