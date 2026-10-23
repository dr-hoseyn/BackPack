// Package testhistory stores bounded, credential-free Connection Test samples.
package testhistory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"time"
)

const Limit = 100
const maxFile = 256 << 10

var filename = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[a-f0-9]{16}\.json$`)
var identity = regexp.MustCompile(`^[A-Za-z0-9.@_-]{0,253}$`)

type Measurement struct {
	Kind           string  `json:"kind"`
	Transport      string  `json:"transport"`
	SNI            string  `json:"sni,omitempty"`
	Status         string  `json:"status"`
	Sent           int     `json:"sent"`
	Received       int     `json:"received"`
	RTTms          float64 `json:"rtt_ms"`
	JitterMs       float64 `json:"jitter_ms"`
	JitterMeasured bool    `json:"jitter_measured"`
	Download       float64 `json:"download_mbps"`
	Upload         float64 `json:"upload_mbps"`
	SpeedStable    bool    `json:"speed_stable"`
}
type Path struct {
	Host   string        `json:"host"`
	Peer   string        `json:"peer,omitempty"`
	Family string        `json:"family"`
	State  string        `json:"state"`
	Rows   []Measurement `json:"rows"`
}
type Run struct {
	Mode     string    `json:"mode,omitempty"`
	Version  int       `json:"version"`
	At       time.Time `json:"at"`
	Side     string    `json:"side"`
	Preset   string    `json:"preset"`
	Complete bool      `json:"complete"`
	Paths    []Path    `json:"paths"`
}
type Store struct{ Dir string }

func validate(r Run) error {
	if (r.Mode != "" && r.Mode != "quick" && r.Mode != "full") || r.Version != 1 || r.At.IsZero() || (r.Side != "iran" && r.Side != "kharej") || !identity.MatchString(r.Preset) || len(r.Paths) == 0 || len(r.Paths) > 2 {
		return errors.New("invalid connection test history run")
	}
	for _, p := range r.Paths {
		ip := net.ParseIP(p.Host)
		if ip == nil || p.Peer != "" && net.ParseIP(p.Peer) == nil || (p.Family != "IPv4" && p.Family != "IPv6") || (ip.To4() != nil) != (p.Family == "IPv4") || !identity.MatchString(p.State) || len(p.Rows) > 100 {
			return errors.New("invalid history path")
		}
		for _, m := range p.Rows {
			if !slices.Contains([]string{"reverse", "direct", "spoof"}, m.Kind) || m.Transport == "" || !identity.MatchString(m.Transport) || !identity.MatchString(m.SNI) || !slices.Contains([]string{"ok", "unstable", "down", "skipped", "testing", "screened"}, m.Status) || m.Sent < 0 || m.Sent > 1000000 || m.Received < 0 || m.Received > m.Sent {
				return errors.New("invalid history measurement")
			}
			for _, n := range []float64{m.RTTms, m.JitterMs, m.Download, m.Upload} {
				if n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
					return errors.New("invalid history number")
				}
			}
		}
	}
	return nil
}

// Save uses a unique file per run: overlapping CLI processes cannot overwrite
// each other. Neither links, raw diagnostic errors nor config structs enter it.
func (s Store) Save(r Run) error {
	r.Version = 1
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	if err := validate(r); err != nil {
		return err
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if len(body) > maxFile {
		return errors.New("history run exceeds size limit")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".sample-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	name := r.At.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(nonce[:]) + ".json"
	if err = os.Rename(f.Name(), filepath.Join(s.Dir, name)); err != nil {
		return err
	}
	names, err := s.files()
	if err != nil {
		return err
	}
	for _, name := range names[min(Limit, len(names)):] {
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
func (s Store) files() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && filename.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// Load returns valid samples even if another file is corrupt, with a warning.
// Bounded reads and skipping symlinks keep a hand-edited store from following
// another file or consuming arbitrary memory. Missing files during pruning are normal.
func (s Store) Load() ([]Run, error) {
	names, err := s.files()
	if err != nil {
		return nil, err
	}
	runs := make([]Run, 0, min(Limit, len(names)))
	var warnings []error
	for _, name := range names[:min(Limit, len(names))] {
		f, err := os.Open(filepath.Join(s.Dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			warnings = append(warnings, err)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(f, maxFile+1))
		f.Close()
		var r Run
		if err == nil && len(body) <= maxFile {
			err = json.Unmarshal(body, &r)
		} else if err == nil {
			err = errors.New("file exceeds size limit")
		}
		if err == nil {
			err = validate(r)
		}
		if err != nil {
			warnings = append(warnings, fmt.Errorf("history sample %s: %w", name, err))
			continue
		}
		runs = append(runs, r)
	}
	return runs, errors.Join(warnings...)
}

type Summary struct {
	Mode          string  `json:"mode,omitempty"`
	Side          string  `json:"side"`
	Preset        string  `json:"preset"`
	Host          string  `json:"host"`
	Peer          string  `json:"peer,omitempty"`
	Family        string  `json:"family"`
	Kind          string  `json:"kind"`
	Transport     string  `json:"transport"`
	SNI           string  `json:"sni,omitempty"`
	Runs          int     `json:"runs"`
	Steady        int     `json:"steady"`
	Sent          int     `json:"sent"`
	Received      int     `json:"received"`
	Loss          float64 `json:"loss_pct"`
	RTTms         float64 `json:"rtt_ms"`
	JitterMs      float64 `json:"jitter_ms"`
	Download      float64 `json:"median_download_mbps"`
	Upload        float64 `json:"median_upload_mbps"`
	SpeedSamples  int     `json:"speed_samples"`
	JitterSamples int     `json:"jitter_samples"`
}

// Compare groups only the same peer, IP family, side, preset, direction,
// protocol and SNI. Partial runs and skipped rows never become stability evidence.
// Loss is weighted by attempted echoes, throughput is the median of completed
// healthy transfers; the sample's success ratio is not an uptime percentage.
func Compare(runs []Run) []Summary {
	type group struct {
		Summary
		down, up     []float64
		rtt, jitter  float64
		jitterEchoes int
	}
	groups := map[[9]string]*group{}
	for _, r := range runs {
		mode := r.Mode
		if mode == "" {
			mode = "full"
		}
		if !r.Complete {
			continue
		}
		for _, p := range r.Paths {
			if p.State != "done" {
				continue
			}
			for _, m := range p.Rows {
				if m.Status == "skipped" || m.Status == "testing" || m.Status == "screened" {
					continue
				}
				key := [9]string{mode, r.Side, r.Preset, p.Host, p.Peer, p.Family, m.Kind, m.Transport, m.SNI}
				g := groups[key]
				if g == nil {
					g = &group{Summary: Summary{Mode: mode, Side: r.Side, Preset: r.Preset, Host: p.Host, Peer: p.Peer, Family: p.Family, Kind: m.Kind, Transport: m.Transport, SNI: m.SNI}}
					groups[key] = g
				}
				g.Runs++
				if m.Status == "ok" {
					g.Steady++
				}
				g.Sent += m.Sent
				g.Received += m.Received
				g.rtt += m.RTTms * float64(m.Received)
				if m.JitterMeasured {
					g.jitter += m.JitterMs * float64(m.Received)
					g.jitterEchoes += m.Received
					g.JitterSamples++
				}
				if m.Status == "ok" && m.Download > 0 && m.Upload > 0 {
					g.down = append(g.down, m.Download)
					g.up = append(g.up, m.Upload)
				}
			}
		}
	}
	median := func(a []float64) float64 {
		if len(a) == 0 {
			return 0
		}
		sort.Float64s(a)
		n := len(a)
		return (a[(n-1)/2] + a[n/2]) / 2
	}
	result := make([]Summary, 0, len(groups))
	for _, g := range groups {
		if g.Sent > 0 {
			g.Loss = 100 * float64(g.Sent-g.Received) / float64(g.Sent)
		}
		if g.Received > 0 {
			g.RTTms = g.rtt / float64(g.Received)

		}
		if g.jitterEchoes > 0 {
			g.JitterMs = g.jitter / float64(g.jitterEchoes)
		}
		g.Download = median(g.down)
		g.Upload = median(g.up)
		g.SpeedSamples = len(g.down)
		result = append(result, g.Summary)
	}
	sort.Slice(result, func(i, j int) bool {
		a, _ := json.Marshal(result[i])
		b, _ := json.Marshal(result[j])
		return string(a) < string(b)
	})
	return result
}
