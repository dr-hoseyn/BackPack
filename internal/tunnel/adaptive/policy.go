// Package adaptive keeps managed carriers warm behind a stable Reverse engine.
// Selection changes affect new tunnel connections; idle pool connections are
// retired while active forwards remain on the connection they already own.
package adaptive

import (
	"errors"
	"math"
	"sync"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/pathprobe"
)

func policyDefaults(p config.PathPolicy) (config.PathPolicy, error) {
	for _, v := range []struct {
		value    int
		min, max int
	}{
		{p.ProbeInterval, 1, 300}, {p.ProbeTimeout, 1, 30}, {p.ProbeBytes, 1024, 65536}, {p.Failures, 1, 10}, {p.Recoveries, 1, 10}, {p.MinDwell, 1, 3600}, {p.Improvement, 1, 90},
	} {
		if v.value != 0 && (v.value < v.min || v.value > v.max) {
			return p, errors.New("path_policy value is outside its documented bounds")
		}
	}
	if p.ProbeInterval == 0 {
		p.ProbeInterval = 5
	}
	if p.ProbeTimeout == 0 {
		p.ProbeTimeout = 5
	}
	if p.ProbeBytes == 0 {
		p.ProbeBytes = 16384
	}
	if p.Failures == 0 {
		p.Failures = 2
	}
	if p.Recoveries == 0 {
		p.Recoveries = 2
	}
	if p.MinDwell == 0 {
		p.MinDwell = 30
	}
	if p.Improvement == 0 {
		p.Improvement = 20
	}
	return p, nil
}

type pathState struct {
	healthy             bool
	seen                bool
	failed              bool
	successes, failures int
	rtt, speed, loss    float64
	checked             time.Time
}

type Switch struct {
	Old, New int
	Datagram bool
}

type Selector struct {
	mu       sync.Mutex
	policy   config.PathPolicy
	states   []pathState
	native   []bool
	selected [2]int
	changed  [2]time.Time
}

func NewSelector(p config.PathPolicy, native []bool) (*Selector, error) {
	p, err := policyDefaults(p)
	if err != nil {
		return nil, err
	}
	return &Selector{policy: p, states: make([]pathState, len(native)), native: append([]bool(nil), native...), selected: [2]int{-1, -1}}, nil
}

func (s *Selector) score(i int) float64 {
	p := s.states[i]
	if !p.healthy || p.speed <= 0 {
		return math.Inf(1)
	}
	return (p.rtt + float64(s.policy.ProbeBytes*2)/p.speed*float64(time.Second)) * (1 + 4*p.loss)
}

func (s *Selector) Record(i int, result pathprobe.Result, err error, now time.Time) []Switch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.states) {
		return nil
	}
	p := &s.states[i]
	p.checked = now
	if err != nil || result.RTT <= 0 || result.BytesPerSecond <= 0 || math.IsNaN(result.BytesPerSecond) || math.IsInf(result.BytesPerSecond, 0) {
		p.failures++
		p.successes = 0
		p.loss = p.loss*.75 + .25
		if p.failures >= s.policy.Failures {
			p.healthy = false
			p.failed = true
		}
	} else {
		p.successes++
		p.failures = 0
		p.loss *= .75
		if !p.seen {
			p.rtt = float64(result.RTT)
			p.speed = result.BytesPerSecond
		} else {
			p.rtt = p.rtt*.75 + float64(result.RTT)*.25
			p.speed = p.speed*.75 + result.BytesPerSecond*.25
		}
		p.seen = true
		if !p.failed || p.successes >= s.policy.Recoveries {
			p.healthy = true
			p.failed = false
		}
	}
	return s.selectLocked(now)
}

// Withdraw removes a locally dead helper immediately. Unlike a remote probe
// failure, process exit is conclusive and must not wait for Failures or dwell.
// A restarted helper still needs Recoveries verified transfers to return.
func (s *Selector) Withdraw(i int, now time.Time) []Switch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.states) {
		return nil
	}
	p := &s.states[i]
	p.healthy, p.failed = false, true
	p.successes = 0
	p.failures = max(p.failures, s.policy.Failures)
	p.checked = now
	return s.selectLocked(now)
}

func (s *Selector) selectLocked(now time.Time) []Switch {
	var switches []Switch
	for class := range 2 {
		best := -1
		for j := range s.states {
			if class == 1 && !s.native[j] {
				continue
			}
			if s.states[j].healthy && (best < 0 || s.score(j) < s.score(best)) {
				best = j
			}
		}
		old := s.selected[class]
		if old == best {
			continue
		}
		if old >= 0 && s.states[old].healthy {
			if best < 0 || now.Sub(s.changed[class]) < time.Duration(s.policy.MinDwell)*time.Second {
				continue
			}
			if s.score(best) > s.score(old)*(1-float64(s.policy.Improvement)/100) {
				continue
			}
		}
		s.selected[class] = best
		s.changed[class] = now
		switches = append(switches, Switch{old, best, class == 1})
	}
	return switches
}

func (s *Selector) Pick(datagram bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	class := 0
	if datagram {
		class = 1
	}
	return s.selected[class]
}

type PathStatus struct {
	Healthy        bool      `json:"healthy"`
	RTTMillis      float64   `json:"rtt_ms"`
	Mbps           float64   `json:"mbps"`
	FailurePercent float64   `json:"failure_percent"`
	Failures       int       `json:"consecutive_failures"`
	Checked        time.Time `json:"checked"`
}

func (s *Selector) Status() []PathStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]PathStatus, len(s.states))
	for i, p := range s.states {
		result[i] = PathStatus{p.healthy, p.rtt / float64(time.Millisecond), p.speed * 8 / 1e6, p.loss * 100, p.failures, p.checked}
	}
	return result
}
