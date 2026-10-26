package network

import (
	"context"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Health scoring for a multi-exit tunnel.
//
// A rotating endpoint list already fails over when a dial fails outright, but a
// route rarely dies cleanly — far more often it just gets worse: latency climbs,
// jitter sets in, a few percent of packets start dropping. None of that trips a
// failed dial, so without scoring the tunnel stays on a degrading exit until it
// finally breaks. This measures every exit and steers traffic to the healthiest.
//
// The score is the one a real-time link is felt through, not raw latency:
//
//	score = mean_rtt_ms + 2·jitter_ms + 20·loss_percent      (lower is better)
//
// Jitter and loss are weighted far above latency because a steady 90 ms exit
// beats a 60 ms one that stutters — a stutter is a lost frame, a spike in latency
// is not. It is the same weighting hydra proved on these routes.

// EndpointScore is one exit's measured health.
type EndpointScore struct {
	Addr      string
	RTTms     float64
	JitterMs  float64
	LossPct   float64
	Score     float64
	Reachable bool
}

// scoreWeights: jitter counts double, each percent of loss counts twenty times a
// millisecond of latency.
func computeScore(rttMs, jitterMs, lossPct float64) float64 {
	return rttMs + 2*jitterMs + 20*lossPct
}

var (
	// Linux `ping -q` summary: "rtt min/avg/max/mdev = 12.3/14.1/40.2/3.4 ms".
	reScoreRTT  = regexp.MustCompile(`=\s*[0-9.]+/([0-9.]+)/[0-9.]+/([0-9.]+)`)
	reScoreLoss = regexp.MustCompile(`([0-9.]+)% packet loss`)
)

// hostOf strips the port so ICMP has a bare host to ping; an address with no
// port is returned unchanged.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// parseScorePing pulls avg RTT, jitter (ping's mdev) and loss out of a ping
// summary. Split out from the exec so it can be tested without a network.
func parseScorePing(out string) EndpointScore {
	var s EndpointScore
	if m := reScoreLoss.FindStringSubmatch(out); m != nil {
		s.LossPct, _ = strconv.ParseFloat(m[1], 64)
	} else {
		s.LossPct = 100
	}
	if m := reScoreRTT.FindStringSubmatch(out); m != nil {
		s.RTTms, _ = strconv.ParseFloat(m[1], 64)
		s.JitterMs, _ = strconv.ParseFloat(m[2], 64)
		s.Reachable = true
	}
	s.Score = computeScore(s.RTTms, s.JitterMs, s.LossPct)
	return s
}

// ScoreEndpoint measures one exit with a short ICMP burst. An unreachable exit
// comes back with Reachable=false and a score that sorts it last.
func ScoreEndpoint(addr string, samples int) EndpointScore {
	return scoreEndpoint(context.Background(), addr, samples)
}

func scoreEndpoint(ctx context.Context, addr string, samples int) EndpointScore {
	if samples <= 0 {
		samples = 5
	}
	// Bound DNS and ping itself, including installations with a broken ping.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(samples)*200*time.Millisecond+3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ping", "-c", strconv.Itoa(samples), "-i", "0.2", "-W", "2", "-q", hostOf(addr))
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, _ := cmd.CombinedOutput() // non-zero exit on loss is expected; parse anyway
	s := parseScorePing(string(out))
	s.Addr = addr
	if !s.Reachable {
		s.Score = 1e9 // last, but still comparable
	}
	return s
}

// ScoreEndpoints measures every exit and returns them best-first. Handy for the
// operator-facing "which exit should I pin?" view.
func ScoreEndpoints(addrs []string, samples int) []EndpointScore {
	out := make([]EndpointScore, len(addrs))
	for i, a := range addrs {
		out[i] = ScoreEndpoint(a, samples)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out
}

// EnableHealthSteering starts a background loop that scores every endpoint on
// `interval` and steers Current()/Next() to the healthiest, with hysteresis so
// the choice does not flap. It no-ops on a single endpoint, and it turns spread
// off because steering and spreading are opposite intentions. The loop stops
// when ctx is done. `log`, if non-nil, is called with a one-line note whenever
// the preferred exit actually changes. The returned channel closes after the
// loop and its probe workers have stopped.
func (e *Endpoints) EnableHealthSteering(ctx context.Context, interval time.Duration, log func(string)) <-chan struct{} {
	stopped := make(chan struct{})
	if e == nil || len(e.list) <= 1 {
		close(stopped)
		return stopped
	}
	e.spread.Store(false)
	e.preferred.Store(e.idx.Load())
	e.steer.Store(true)
	go func() {
		defer close(stopped)
		e.steerLoop(ctx, interval, log)
	}()
	return stopped
}

// steerLoop is the scoring cadence with its hysteresis. A challenger only takes
// over when it is clearly better (≥15%) for several checks in a row, so a single
// bad sample on the current exit cannot bounce traffic away and back. The one
// exception is a preferred exit that has gone unreachable: there is nothing to
// protect, so the switch is immediate.
func (e *Endpoints) steerLoop(ctx context.Context, interval time.Duration, log func(string)) {
	defer e.steer.Store(false)
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	choice := healthChoice{candidate: -1, incumbent: -1}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		probe, switched := e.healthProbe.Load(), e.healthSwitch.Load()
		scores := e.measureHealth(ctx, probe)
		if ctx.Err() != nil {
			return
		}
		if probe != e.healthProbe.Load() || switched != e.healthSwitch.Load() {
			choice = healthChoice{candidate: -1, incumbent: -1}
			continue // the transport changed while this batch was being measured
		}
		e.applyHealthScores(ctx, &choice, scores, log)
	}
}

// Probe at most four routes at once. One blackholed IPv4 endpoint must not
// delay measuring every IPv6 backup by its entire timeout.
func (e *Endpoints) measureHealth(ctx context.Context, probe *func(context.Context, string, int) EndpointScore) []EndpointScore {
	scores := make([]EndpointScore, len(e.list))
	jobs := make(chan int)
	var workers sync.WaitGroup
	reach := e.reach.Load()
	for n := 0; n < min(4, len(e.list)); n++ {
		workers.Go(func() {
			for i := range jobs {
				if probe != nil {
					scores[i] = (*probe)(ctx, e.list[i], 5)
				} else {
					scores[i] = scoreEndpoint(ctx, e.list[i], 5)
				}
				if scores[i].Reachable && reach != nil && !(*reach)(e.list[i]) {
					scores[i].Reachable, scores[i].Score = false, 1e9
				}
			}
		})
	}
	for i := range e.list {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return scores
		}
	}
	close(jobs)
	workers.Wait()
	return scores
}

// healthChoice requires three consecutive improvements against the same
// incumbent. Losing all probes or a dial selecting another endpoint resets it.
type healthChoice struct {
	candidate, incumbent, streak int
}

func (h *healthChoice) choose(scores []EndpointScore, cur int) (int, bool) {
	best := -1
	for i, s := range scores {
		if s.Reachable && (best < 0 || s.Score < scores[best].Score) {
			best = i
		}
	}
	if best < 0 || best == cur || (scores[cur].Reachable &&
		(scores[best].Score >= scores[cur].Score || scores[best].Score > 0.85*scores[cur].Score)) {
		h.candidate, h.incumbent, h.streak = -1, cur, 0
		return cur, false
	}
	if h.candidate == best && h.incumbent == cur {
		h.streak++
	} else {
		h.candidate, h.incumbent, h.streak = best, cur, 1
	}
	if !scores[cur].Reachable || h.streak >= 3 {
		h.candidate, h.incumbent, h.streak = -1, best, 0
		return best, true
	}
	return cur, false
}

func (e *Endpoints) applyHealthScores(ctx context.Context, choice *healthChoice, scores []EndpointScore, log func(string)) {
	previous := e.preferred.Load()
	cur := e.clampIdx(previous)
	best, change := choice.choose(scores, cur)
	// Do not overwrite a dial's preference if it changes during this decision.
	if !change || ctx.Err() != nil || !e.preferred.CompareAndSwap(previous, int64(best)) {
		return
	}
	e.idx.Store(int64(best))
	if log != nil {
		log("failover: steering from " + e.list[cur] + " to healthiest exit " + e.list[best] +
			" (score " + strconv.FormatFloat(scores[best].Score, 'f', 0, 64) + ")")
	}
	if f := e.healthSwitch.Load(); f != nil {
		(*f)(e.list[cur], e.list[best])
	}
}

// TCPReach reports whether addr takes a TCP connection within timeout: the
// reach probe for a tunnel whose port is TCP. See SetReachProbe.
func TCPReach(timeout time.Duration) func(addr string) bool {
	return func(addr string) bool {
		c, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
}
