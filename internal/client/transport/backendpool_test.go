package transport

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A single backend is a passthrough: no pipe, no health-checking, unchanged.
func TestBackendPoolSinglePassthrough(t *testing.T) {
	p := &backendPool{groups: map[string]*backendGroup{}}
	if got := p.candidates("127.0.0.1:8443"); !slices.Equal(got, []string{"127.0.0.1:8443"}) {
		t.Fatalf("single backend = %q, want unchanged", got)
	}
	if len(p.groups) != 0 {
		t.Fatal("a single backend must not create a health-checked group")
	}
}

// Round-robin spreads over the healthy backends and skips a dropped one.
func TestBackendGroupPrefersHealthyBackends(t *testing.T) {
	g := &backendGroup{
		list:    []string{"a:1", "b:2", "c:3"},
		healthy: []bool{true, false, true}, // b is down
		fails:   make([]int, 3),
		// Recent enough that candidates() does not start a real probe pass.
		lastCheck: time.Now(),
	}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		order := g.candidates()
		if len(order) != 3 || order[2] != "b:2" || order[0] == order[1] {
			t.Fatalf("dial order = %v, want both healthy backends before the unhealthy one", order)
		}
		seen[order[0]]++
	}
	if seen["b:2"] != 0 {
		t.Fatalf("picked the unhealthy backend: %v", seen)
	}
	if seen["a:1"] == 0 || seen["c:3"] == 0 {
		t.Fatalf("did not spread over the healthy backends: %v", seen)
	}
}

// The health checker drops a backend after MaxFailed and restores it on recovery.
func TestBackendHealthCheckDropsAndRestores(t *testing.T) {
	// "down:0" always fails, everything else answers.
	probe := func(addr string) bool { return addr != "down:0" }

	g := &backendGroup{
		list:      []string{"up:1", "down:0"},
		healthy:   []bool{true, true},
		fails:     make([]int, 2),
		lastCheck: time.Now(),
	}
	// Run enough probe rounds to cross the failure threshold.
	for i := 0; i < backendMaxFailed; i++ {
		g.check(probe)
	}
	g.mu.Lock()
	dropped := !g.healthy[1] && g.healthy[0]
	g.mu.Unlock()
	if !dropped {
		t.Fatal("unhealthy backend was not dropped from rotation")
	}
	// The healthy one must now be first.
	for i := 0; i < 4; i++ {
		if got := g.candidates()[0]; got != "up:1" {
			t.Fatalf("pick = %q, want only the healthy up:1", got)
		}
	}

	// One answering pass puts it straight back in the rotation.
	g.check(func(string) bool { return true })
	g.mu.Lock()
	restored := g.healthy[1] && g.fails[1] == 0
	g.mu.Unlock()
	if !restored {
		t.Fatal("a recovered backend was not restored to the rotation")
	}
}

// A target with a pipe in it but nothing usable on either side of it used to
// build a group with an empty list, and the round-robin's `% 0` took the
// process down with it.
func TestBackendPoolSurvivesEmptyAndSingletonLists(t *testing.T) {
	p := &backendPool{groups: map[string]*backendGroup{}}

	if got := p.candidates("||"); !slices.Equal(got, []string{"||"}) {
		t.Fatalf("empty backend list = %q, want the target unchanged", got)
	}
	if got := p.candidates(" 127.0.0.1:8443 | "); !slices.Equal(got, []string{"127.0.0.1:8443"}) {
		t.Fatalf("one real backend = %q, want it trimmed and returned", got)
	}
	if len(p.groups) != 0 {
		t.Fatalf("nothing to balance created %d groups", len(p.groups))
	}
}

// Spacing and a trailing separator describe the same two backends, so they
// must share one group rather than each getting their own.
func TestBackendPoolKeysOnTheParsedList(t *testing.T) {
	p := &backendPool{groups: map[string]*backendGroup{}}
	p.candidates("a:1|b:2")
	p.candidates(" a:1 | b:2 |")
	if len(p.groups) != 1 {
		t.Fatalf("equivalent targets created %d groups, want 1", len(p.groups))
	}
}

// The targets these keys come from arrive over the tunnel, so the map has to
// stop growing at some point rather than trusting the far end.
func TestBackendPoolGroupCacheIsBounded(t *testing.T) {
	p := &backendPool{groups: map[string]*backendGroup{}}
	for i := 0; i < backendMaxGroups+50; i++ {
		list := []string{fmt.Sprintf("a-%d:1", i), fmt.Sprintf("b-%d:2", i)}
		p.group(list)
	}
	if len(p.groups) != backendMaxGroups {
		t.Fatalf("group cache holds %d entries, want at most %d", len(p.groups), backendMaxGroups)
	}
}

// A list longer than the member bound is truncated rather than allocated in
// full — and the group it builds is still a working rotation.
func TestBackendPoolBoundsMembersPerGroup(t *testing.T) {
	p := &backendPool{groups: map[string]*backendGroup{}}
	target := ""
	for i := 0; i < backendMaxMembers+20; i++ {
		if i > 0 {
			target += backendSep
		}
		target += fmt.Sprintf("host-%d:1", i)
	}
	if got := p.candidates(target); len(got) != backendMaxMembers {
		t.Fatalf("an over-long backend list produced %d candidates, want %d", len(got), backendMaxMembers)
	}
	if len(p.groups) != 1 {
		t.Fatalf("group count = %d, want 1", len(p.groups))
	}
	for _, g := range p.groups {
		if len(g.list) != backendMaxMembers {
			t.Fatalf("group holds %d members, want %d", len(g.list), backendMaxMembers)
		}
	}
}

func TestUDPBackendSelectionPreservesTheFirstUsableAddress(t *testing.T) {
	for _, tc := range []struct{ target, want string }{
		{"127.0.0.1:8443", "127.0.0.1:8443"},
		{"  [::1]:8443  ", "[::1]:8443"},
		{" | \t | 127.0.0.1:8443 | [::1]:8444", "127.0.0.1:8443"},
		{"", ""},
		{" | \t | ", " | \t | "},
	} {
		if got := firstBackend(tc.target); got != tc.want {
			t.Fatalf("first backend of %q = %q, want %q", tc.target, got, tc.want)
		}
	}
	target := " | 127.0.0.1:8443 |" + strings.Repeat("ignored:9|", 4096)
	if got := testing.AllocsPerRun(100, func() {
		if firstBackend(target) != "127.0.0.1:8443" {
			t.Fatal("UDP selected a later backend")
		}
	}); got != 0 {
		t.Fatalf("selecting a UDP backend allocated %.1f objects", got)
	}
}

func TestBackendParsingBoundsWorkBeforeDiscardingExcessMembers(t *testing.T) {
	var prefix []string
	for i := 0; i < backendMaxMembers; i++ {
		prefix = append(prefix, fmt.Sprintf("host-%d:8443", i))
	}
	// Empty and whitespace-only fields must not use up real member slots.
	target := " | " + strings.Join(prefix, " | | ") + " | "
	oversized := target + strings.Repeat("ignored:9|", 4096)
	for _, input := range []string{target, oversized} {
		if got := splitBackends(input); !slices.Equal(got, prefix) {
			t.Fatalf("bounded parsing changed backend order: got %v, want %v", got, prefix)
		}
	}
	allocs := func(input string) float64 {
		return testing.AllocsPerRun(100, func() {
			if len(splitBackends(input)) != backendMaxMembers {
				t.Fatal("parser changed its member limit")
			}
		})
	}
	if short, long := allocs(target), allocs(oversized); long != short {
		t.Fatalf("ignored suffix changed allocations: %.1f for bounded list, %.1f for oversized list", short, long)
	}
}
