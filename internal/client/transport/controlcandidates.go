package transport

import "github.com/backpack/backpack/internal/utils/network"

// A TCP accept is not proof of a working control handshake. Try every other
// address before revisiting a socket that accepted but never spoke the protocol.
// Explicit authentication refusals do not reach this path. A completed sweep
// resets the exclusions, preserving recovery and legacy handshake negotiation.
func controlCandidates(e *network.Endpoints, failed *map[string]bool) []string {
	all := e.InPreferenceOrder()
	if len(all) < 2 || len(*failed) == 0 {
		return all
	}
	out := make([]string, 0, len(all))
	for _, a := range all {
		if !(*failed)[a] {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		*failed = nil
		return all
	}
	return out
}
func rejectControlCandidate(failed *map[string]bool, addr string) {
	if *failed == nil {
		*failed = make(map[string]bool)
	}
	(*failed)[addr] = true
}
