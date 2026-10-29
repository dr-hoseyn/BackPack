package transport

import (
	"github.com/backpack/backpack/internal/utils/network"
	"testing"
)

func TestReviewedControlCandidatesTryAllHandshakesBeforeRepeating(t *testing.T) {
	e := network.NewEndpoints("primary", "second", "third")
	var failed map[string]bool
	for _, bad := range []string{"primary", "second"} {
		rejectControlCandidate(&failed, bad)
		for _, a := range controlCandidates(e, &failed) {
			if failed[a] {
				t.Fatal("failed handshake remained eligible")
			}
		}
	}
	if got := controlCandidates(e, &failed); len(got) != 1 || got[0] != "third" {
		t.Fatalf("candidates %v", got)
	}
	rejectControlCandidate(&failed, "third")
	if got := controlCandidates(e, &failed); len(got) != 3 || failed != nil {
		t.Fatal("completed sweep did not reset")
	}
}
