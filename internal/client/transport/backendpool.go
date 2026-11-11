package transport

import (
	"net"
	"strings"
	"sync"
	"time"
)

// Backend pool: health-checked load balancing across several local services.
//
// A forwarded port can name more than one backend, separated by a pipe
// (`443=127.0.0.1:8443|127.0.0.1:8444`). Connections are spread over the
// backends that are up, and a backend that stops answering is dropped from the
// rotation until it recovers — so one dead service behind the tunnel no longer
// takes the tunnel down with it.
//
// The separator is a pipe rather than a comma because a comma already separates
// whole port entries ("443,8080"), so a comma here would split one mapping into
// two.
//
// A single backend (no pipe) is returned unchanged and never health-checked,
// so every existing tunnel behaves exactly as before: this is inert until a
// second backend is configured.
//
// The check is a plain TCP connect (item 10's active health check) and the
// pick is round-robin over the healthy set (item 9's load balancing) — the two
// are the same mechanism here, because the check is what decides the set the
// balancer draws from.

// backendSep separates several backends inside one port mapping.
const backendSep = "|"

const (
	backendCheckInterval = 10 * time.Second
	backendProbeTimeout  = 3 * time.Second
	backendMaxFailed     = 3 // consecutive failures before a backend is dropped

	// The target a backend list is derived from arrives over the tunnel, not
	// out of this side's config, so its shape is not something this process
	// gets to assume. These two bounds are what keep a hostile or simply broken
	// peer from turning that into unbounded memory here. Both sit far above any
	// real deployment: a forwarded port with more than 32 services behind it,
	// or a client with more than 256 distinct backend lists in flight, is not a
	// configuration anyone writes.
	backendMaxGroups  = 256
	backendMaxMembers = 32
)

var backends = &backendPool{groups: map[string]*backendGroup{}}

type backendPool struct {
	mu     sync.Mutex
	groups map[string]*backendGroup
}

// candidates returns backends in dial order, with a round-robin healthy
// backend first when one is available.
//
// A target only becomes a group once it has survived parsing into two or more
// real addresses. "a:1|" is one backend and "||" is none, and neither has
// anything to balance — the old code counted the pipe rather than the result,
// so a mapping like "||" built a group with an empty list and the round-robin
// then took `g.next % 0` and brought the process down.
func (p *backendPool) candidates(target string) []string {
	if !strings.Contains(target, backendSep) {
		return []string{target}
	}
	list := splitBackends(target)
	switch {
	case len(list) == 0:
		return []string{target} // nothing usable; let the dial fail and be reported
	case len(list) == 1:
		return list
	}
	// Key on the parsed list rather than the raw target so that spacing and a
	// trailing separator do not each get a pool of their own.
	return p.group(list).candidates()
}

// group returns the pool for a parsed backend list, creating it the first time
// that list is seen. The map is bounded and evicts the group used longest ago,
// because what goes into it is decided by the far end of the tunnel.
func (p *backendPool) group(list []string) *backendGroup {
	key := strings.Join(list, backendSep)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.groups == nil {
		p.groups = make(map[string]*backendGroup)
	}
	if g, ok := p.groups[key]; ok {
		g.touch()
		return g
	}
	if len(p.groups) >= backendMaxGroups {
		p.evictOldestLocked()
	}
	g := &backendGroup{
		// Reference the bounded canonical key, not slices of the raw target.
		// Otherwise an ignored suffix stays alive for the group's lifetime.
		list:     strings.Split(key, backendSep),
		healthy:  make([]bool, len(list)),
		fails:    make([]int, len(list)),
		lastUsed: time.Now(),
	}
	for i := range g.healthy {
		g.healthy[i] = true // optimistic until the first check proves otherwise
	}
	p.groups[key] = g
	return g
}

// evictOldestLocked drops the least recently used group. Called with p.mu held,
// and only on the rare insert that would push the map past its bound, so the
// scan costs nothing in the steady state.
func (p *backendPool) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, g := range p.groups {
		used := g.lastUsedAt()
		if oldestKey == "" || used.Before(oldest) {
			oldestKey, oldest = key, used
		}
	}
	delete(p.groups, oldestKey)
}

type backendGroup struct {
	list      []string
	mu        sync.Mutex
	healthy   []bool
	fails     []int
	next      int
	checking  bool
	lastCheck time.Time
	lastUsed  time.Time
}

func (g *backendGroup) touch() {
	g.mu.Lock()
	g.lastUsed = time.Now()
	g.mu.Unlock()
}

func (g *backendGroup) lastUsedAt() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastUsed
}

// candidates advances the rotation once per incoming connection. A backend
// marked down can still be used as a fallback if it recovered between checks.
func (g *backendGroup) candidates() []string {
	g.startCheck()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastUsed = time.Now()
	n := len(g.list)
	start := g.next
	order := make([]string, 0, n)
	for _, healthy := range []bool{true, false} {
		for i := 0; i < n; i++ {
			idx := (start + i) % n
			if g.healthy[idx] == healthy {
				order = append(order, g.list[idx])
				if len(order) == 1 {
					g.next = (idx + 1) % n
				}
			}
		}
	}
	return order
}

// startCheck runs one health-check pass, at most one at a time and at most
// once per backendCheckInterval.
//
// This used to be a goroutine per group looping forever, which meant a group
// was never collectible and never stopped probing — it outlived the tunnel
// generation that created it and went on dialling backends nothing was routing
// to. Driving the checks from candidates() instead ties the probing to use:
// an abandoned group finishes its current pass and then does nothing at all.
// The cost is that a backend that recovers while no traffic is flowing is not
// noticed until the next connection arrives, which is the moment the answer
// starts to matter anyway.
func (g *backendGroup) startCheck() {
	g.mu.Lock()
	if g.checking || time.Since(g.lastCheck) < backendCheckInterval {
		g.mu.Unlock()
		return
	}
	g.checking = true
	g.lastCheck = time.Now()
	g.mu.Unlock()

	go func() {
		g.check(backendProbe)
		g.mu.Lock()
		g.checking = false
		g.lastCheck = time.Now()
		g.mu.Unlock()
	}()
}

// check probes every backend once, dropping one after backendMaxFailed
// consecutive failures and restoring it the moment it answers again.
func (g *backendGroup) check(probe func(string) bool) {
	for i, b := range g.list {
		ok := probe(b)
		g.mu.Lock()
		if ok {
			g.healthy[i], g.fails[i] = true, 0
		} else {
			g.fails[i]++
			if g.fails[i] >= backendMaxFailed {
				g.healthy[i] = false
			}
		}
		g.mu.Unlock()
	}
}

// backendProbe is a plain TCP connect.
func backendProbe(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, backendProbeTimeout)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// firstBackend returns the first address of a backend list, or the input
// unchanged. Used by the UDP transport, which cannot be health-checked with a
// TCP probe, so it does not load-balance — it just tolerates a list.
func firstBackend(target string) string {
	// UDP uses only the first usable member. Do not allocate or parse the
	// rest of a TCP backend list just to discard it.
	for part := range strings.SplitSeq(target, backendSep) {
		if part = strings.TrimSpace(part); part != "" {
			return part
		}
	}
	return target
}

// splitBackends keeps the first backendMaxMembers trimmed, non-empty addresses.
// Stop while parsing: splitting the whole peer-supplied target and truncating
// afterwards still allocated for every ignored member and separator.
func splitBackends(list string) []string {
	// Collect into bounded stack storage, then allocate only the returned
	// members. Growing a slice repeatedly costs more allocations at the cap.
	var members [backendMaxMembers]string
	n := 0
	for p := range strings.SplitSeq(list, backendSep) {
		if p = strings.TrimSpace(p); p != "" {
			members[n] = p
			n++
			if n == backendMaxMembers {
				break
			}
		}
	}
	return append([]string(nil), members[:n]...)
}
