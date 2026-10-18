package manage

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/backpack/backpack/internal/app"
	"github.com/backpack/backpack/internal/testhistory"
	"github.com/backpack/backpack/internal/tui"
)

// ConnTestHistoryRoot can be redirected by tests without touching live state.
var ConnTestHistoryRoot = filepath.Join(app.ConfigDir, "connection-tests")

func LoadConnTestHistory() ([]testhistory.Run, error) {
	return (testhistory.Store{Dir: ConnTestHistoryRoot}).Load()
}

func ctSaveHistory(side, preset string, paths []ConnTestPath, complete bool) {
	r := testhistory.Run{At: time.Now().UTC(), Side: side, Preset: preset, Complete: complete}
	for _, p := range paths {
		if net.ParseIP(p.Host) == nil {
			continue
		}
		path := testhistory.Path{Host: p.Host, Family: p.Family, State: p.State}
		if net.ParseIP(p.Kharej) != nil {
			path.Peer = p.Kharej
		}
		for _, m := range p.Rows {
			path.Rows = append(path.Rows, testhistory.Measurement{Kind: m.Kind, Transport: m.Transport, SNI: m.SNIDomain, Status: m.Status, Sent: m.Tried, Received: m.OK, RTTms: float64(m.RTTms), JitterMs: m.JitterMs, JitterMeasured: m.JitterMeasured, Download: m.Mbps, Upload: m.UploadMbps, SpeedStable: m.SpeedStable})
		}
		r.Paths = append(r.Paths, path)
	}
	if len(r.Paths) == 0 {
		return
	}
	if err := (testhistory.Store{Dir: ConnTestHistoryRoot}).Save(r); err != nil {
		tui.Warn("Could not save test history: " + err.Error())
	}
}
func ctHistoryPreset(paths []ConnTestPath) string {
	for _, p := range paths {
		if p.Best.Preset != "" {
			return p.Best.Preset
		}
	}
	return "unknown"
}

// ConnTestHistoryTable is shared by the terminal menu and scriptable command.
func ConnTestHistoryTable(runs []testhistory.Run, compare bool) string {
	var b strings.Builder
	if len(runs) == 0 {
		return "No saved Connection Tests yet. New terminal tests are saved automatically.\n"
	}
	if !compare {
		fmt.Fprintln(&b, "Saved Connection Tests (newest first; times in UTC):")
		for _, r := range runs {
			state := "complete"
			if !r.Complete {
				state = "partial"
			}
			fmt.Fprintf(&b, "%s  %s  %s  %s\n", r.At.UTC().Format(time.RFC3339), r.Side, r.Preset, state)
			for _, p := range r.Paths {
				fmt.Fprintf(&b, "  %s %s peer=%s  %s  %d protocols\n", p.Family, p.Host, p.Peer, p.State, len(p.Rows))
			}
		}
		return b.String()
	}
	fmt.Fprintln(&b, "Observed samples only; steady runs are not an uptime percentage.")
	fmt.Fprintln(&b, "Partial tests and skipped rows are excluded. Speeds are medians of healthy completed transfers.")
	summaries := testhistory.Compare(runs)
	if len(summaries) == 0 {
		fmt.Fprintln(&b, "No completed measurements to compare.")
	}
	for _, s := range summaries {
		fmt.Fprintf(&b, "%s %s %s peer=%s %s %s %s", s.Side, s.Preset, s.Family, s.Peer, s.Host, s.Kind, ctName(s.Transport))
		if s.SNI != "" {
			fmt.Fprintf(&b, " SNI=%s", s.SNI)
		}
		fmt.Fprintf(&b, "\n  steady %d/%d runs; loss %.2f%% (%d/%d echoes); RTT %.2f ms", s.Steady, s.Runs, s.Loss, s.Received, s.Sent, s.RTTms)
		if s.JitterSamples > 0 {
			fmt.Fprintf(&b, "; jitter %.2f ms (%d samples)", s.JitterMs, s.JitterSamples)
		} else {
			fmt.Fprint(&b, "; jitter not measured")
		}
		if s.SpeedSamples > 0 {
			fmt.Fprintf(&b, "; down %.2f / up %.2f Mb/s (%d speed samples)", s.Download, s.Upload, s.SpeedSamples)
		} else {
			fmt.Fprint(&b, "; speed not measured")
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}
func connTestHistoryMenu() {
	runs, err := LoadConnTestHistory()
	if err != nil {
		tui.Warn(err.Error())
	}
	fmt.Print(ConnTestHistoryTable(runs, false))
	fmt.Print(ConnTestHistoryTable(runs, true))
	tui.PressEnter()
}
