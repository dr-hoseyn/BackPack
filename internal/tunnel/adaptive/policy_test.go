package adaptive

import (
	"errors"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/pathprobe"
)

func TestVerifiedPathSelectionHoldsHealthyPathsButImmediatelyLeavesAConfirmedFailure(t *testing.T) {
	s, err := NewSelector(config.PathPolicy{MinDwell: 30, Improvement: 20, Failures: 2, Recoveries: 2}, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	slow := pathprobe.Result{RTT: 100 * time.Millisecond, BytesPerSecond: 1e6}
	fast := pathprobe.Result{RTT: 10 * time.Millisecond, BytesPerSecond: 10e6}
	s.Record(0, slow, nil, now)
	s.Record(1, fast, nil, now.Add(time.Second))
	if s.Pick(false) != 0 || s.Pick(true) != 0 {
		t.Fatal("healthy dwell or native path eligibility was ignored")
	}
	s.Record(1, fast, nil, now.Add(31*time.Second))
	if s.Pick(false) != 1 {
		t.Fatal("meaningfully faster verified path was not selected")
	}
	failed := errors.New("verified traffic failed")
	s.Record(1, pathprobe.Result{}, failed, now.Add(32*time.Second))
	if s.Pick(false) != 1 {
		t.Fatal("single transient failure caused a switch")
	}
	s.Record(1, pathprobe.Result{}, failed, now.Add(33*time.Second))
	if s.Pick(false) != 0 {
		t.Fatal("failed path was held for the healthy dwell")
	}
	s.Record(1, fast, nil, now.Add(64*time.Second))
	if s.Pick(false) != 0 {
		t.Fatal("path recovered without enough verified successes")
	}
	s.Record(1, fast, nil, now.Add(65*time.Second))
	if s.Pick(false) != 1 {
		t.Fatal("recovered path did not become eligible")
	}
}

func TestFailedOrUnverifiedCandidatesNeverBecomeAFallback(t *testing.T) {
	s, err := NewSelector(config.PathPolicy{}, []bool{false, true})
	if err != nil {
		t.Fatal(err)
	}
	s.Record(0, pathprobe.Result{}, nil, time.Now())
	if s.Pick(false) != -1 || s.Pick(true) != -1 {
		t.Fatal("zero payload measurement marked a path healthy")
	}
	s.Record(1, pathprobe.Result{RTT: time.Millisecond, BytesPerSecond: 100000}, nil, time.Now())
	if s.Pick(false) != 1 || s.Pick(true) != 1 {
		t.Fatal("verified initial path was not selected")
	}
	for range 2 {
		s.Record(1, pathprobe.Result{}, errors.New("unreachable"), time.Now())
	}
	if s.Pick(false) != -1 || s.Pick(true) != -1 {
		t.Fatal("all-down state selected an unverified path")
	}
}

func TestManagedPathPolicyRefusesUnboundedTrafficAndUnusableThresholds(t *testing.T) {
	for _, p := range []config.PathPolicy{{ProbeBytes: 65537}, {ProbeTimeout: 31}, {Failures: -1}, {Recoveries: 11}, {Improvement: 100}, {MinDwell: -1}} {
		if _, err := NewSelector(p, []bool{false, false}); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
