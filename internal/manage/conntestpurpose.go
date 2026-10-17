package manage

import (
	"fmt"
	"math"
	"strings"
)

// ConnTestRecommendation keeps the identity and evidence together: two SNI
// candidates or the same transport over IPv4 and IPv6 are different choices.
// These are observations from one run, not a promise of long-term availability.
type ConnTestRecommendation struct {
	Purpose       string  `json:"purpose"`
	Transport     string  `json:"transport"`
	CaseID        string  `json:"case,omitempty"`
	SNIDomain     string  `json:"sni,omitempty"`
	Host          string  `json:"host,omitempty"`
	Family        string  `json:"family,omitempty"`
	DownloadMbps  float64 `json:"download_mbps"`
	UploadMbps    float64 `json:"upload_mbps"`
	RTTms         float64 `json:"rtt_ms"`
	JitterMs      float64 `json:"jitter_ms"`
	WorstMs       float64 `json:"worst_ms"`
	LossPct       float64 `json:"loss_pct"`
	Samples       int     `json:"samples"`
	SpeedMeasured bool    `json:"speed_measured"`
	SpeedStable   bool    `json:"speed_stable"`
}

var ctPurposes = [...]string{"download", "realtime", "stability"}

func ctPurposeRecommendations(rows []ConnTestResult, cases []*connTestCase) []ConnTestRecommendation {
	var candidates []ConnTestRecommendation
	for i, r := range rows {
		// A quick handshake, a cancelled sample, or a failed speed transfer is
		// never a recommendation, even if an earlier partial speed was high.
		if r.Kind == "spoof" || r.Status != ctOK || r.Phase != "" || r.Tried <= 0 || r.OK <= 0 || r.OK > r.Tried {
			continue
		}
		c := ConnTestRecommendation{
			Transport: r.Kind + " " + r.Transport, CaseID: r.CaseID, SNIDomain: r.SNIDomain,
			DownloadMbps: r.Mbps, UploadMbps: r.UploadMbps, SpeedStable: r.SpeedStable,
		}
		if !ctFiniteSpeed(r.Mbps) || !ctFiniteSpeed(r.UploadMbps) {
			continue
		}
		c.SpeedMeasured = r.Mbps > 0 && r.UploadMbps > 0
		if i < len(cases) && cases[i] != nil && len(cases[i].rtts) == r.OK {
			q := ctQuality(r, cases[i].rtts)
			c.Samples = len(cases[i].rtts)
			c.RTTms = float64(q.Avg) / 1e6
			c.JitterMs = float64(q.Jitter) / 1e6
			c.WorstMs = float64(q.Max) / 1e6
		}
		c.LossPct = float64(r.Tried-r.OK) * 100 / float64(r.Tried)
		candidates = append(candidates, c)
	}
	return ctChoosePurposes(candidates)
}

func ctFiniteSpeed(n float64) bool { return n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }

func ctChoosePurposes(candidates []ConnTestRecommendation) []ConnTestRecommendation {
	var out []ConnTestRecommendation
	for _, purpose := range ctPurposes {
		var best ConnTestRecommendation
		for _, c := range candidates {
			if c.Purpose != "" && c.Purpose != purpose {
				continue
			}
			if purpose == "download" && !c.SpeedMeasured || purpose != "download" && c.Samples < 2 {
				continue
			}
			if best.Transport == "" || ctPurposeBetter(c, best, purpose) {
				best = c
			}
		}
		if best.Transport != "" {
			best.Purpose = purpose
			out = append(out, best)
		}
	}
	return out
}

// Download uses verified receive speed, breaking ties with upload. Games/calls
// prefer lower loss, then RTT + twice the measured jitter. Stability prefers
// lower loss, a converged speed sample, then lower jitter and worst RTT. The
// fixed order makes ties deterministic without favouring a named protocol.
func ctPurposeBetter(a, b ConnTestRecommendation, purpose string) bool {
	if purpose == "download" {
		if a.DownloadMbps != b.DownloadMbps {
			return a.DownloadMbps > b.DownloadMbps
		}
		if a.UploadMbps != b.UploadMbps {
			return a.UploadMbps > b.UploadMbps
		}
	} else {
		if a.LossPct != b.LossPct {
			return a.LossPct < b.LossPct
		}
		if purpose == "stability" {
			// Unknown speed convergence (UDP/older peers) is not evidence of
			// a stable speed sample, nor is it evidence of a failure.
			confidence := func(c ConnTestRecommendation) int {
				if !c.SpeedMeasured {
					return 1
				}
				if c.SpeedStable {
					return 2
				}
				return 0
			}
			if confidence(a) != confidence(b) {
				return confidence(a) > confidence(b)
			}
			if a.JitterMs != b.JitterMs {
				return a.JitterMs < b.JitterMs
			}
			if a.WorstMs != b.WorstMs {
				return a.WorstMs < b.WorstMs
			}
			if a.Samples != b.Samples {
				return a.Samples > b.Samples
			}
		} else if x, y := a.RTTms+2*a.JitterMs, b.RTTms+2*b.JitterMs; x != y {
			return x < y
		}
		if x, y := math.Min(a.DownloadMbps, a.UploadMbps), math.Min(b.DownloadMbps, b.UploadMbps); x != y {
			return x > y
		}
	}
	if a.RTTms != b.RTTms {
		return a.RTTms < b.RTTms
	}
	return false
}

func ctPurposeLabel(purpose string) string {
	switch purpose {
	case "download":
		return "Download"
	case "realtime":
		return "Games / calls"
	case "stability":
		return "Stability"
	}
	return purpose
}

// ConnTestRecommendationsTable is shared by the per-path result and the
// IPv4/IPv6 comparison, so both ends of the test print the same choices.
func ConnTestRecommendationsTable(choices []ConnTestRecommendation) string {
	if len(choices) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("RECOMMENDED BY USE\n")
	for _, c := range choices {
		kind, tr, _ := strings.Cut(c.Transport, " ")
		identity := ctKind(kind) + " " + ctName(tr)
		if c.SNIDomain != "" {
			identity += " · " + c.SNIDomain
		}
		if c.Host != "" {
			identity += " · " + c.Family + " · " + c.Host
		}
		fmt.Fprintf(&out, "  %s: %s\n", ctPurposeLabel(c.Purpose), identity)
		if c.SpeedMeasured {
			fmt.Fprintf(&out, "    down %.2f / up %.2f Mb/s", c.DownloadMbps, c.UploadMbps)
		} else {
			out.WriteString("    speed not measured")
		}
		if c.Samples >= 2 {
			fmt.Fprintf(&out, " · RTT %.1f ms · jitter %.1f ms · loss %.1f%% · %d echoes", c.RTTms, c.JitterMs, c.LossPct, c.Samples)
		}
		if c.SpeedMeasured && !c.SpeedStable {
			out.WriteString(" · speed varied; repeat to confirm")
		}
		out.WriteString("\n")
	}
	out.WriteString("  Based on this sample; stability does not measure long-term uptime.\n\n")
	return out.String()
}
