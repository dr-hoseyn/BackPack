package adaptive

import (
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/pathprobe"
	"sync"
	"testing"
	"time"
)

func TestEconomyBackupPromotionRestoresFastProbes(t *testing.T) {
	s, err := NewSelector(config.PathPolicy{StandbyInterval: 60}, []bool{false, false})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := pathprobe.Result{RTT: time.Millisecond, BytesPerSecond: 1e6}
	s.Record(0, r, nil, now)
	s.Record(1, r, nil, now)
	if s.ProbeDelay(0) != 5*time.Second || s.ProbeDelay(1) != time.Minute {
		t.Fatal("selected/backup cadence")
	}
	s.Withdraw(0, now)
	if s.Pick(false) != 1 || s.ProbeDelay(1) != 5*time.Second || s.ProbeDelay(0) != 5*time.Second {
		t.Fatal("outage must restore fast verification")
	}
	if _, err := NewSelector(config.PathPolicy{ProbeInterval: 20, StandbyInterval: 10}, nil); err == nil {
		t.Fatal("accepted faster standby interval")
	}
}

func TestEconomyStatusSnapshotRemainsCoherentDuringFailover(t *testing.T) {
	s, err := NewSelector(config.PathPolicy{StandbyInterval: 60}, []bool{true, false})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		r := pathprobe.Result{RTT: time.Millisecond, BytesPerSecond: 1e6}
		for range 3000 {
			now := time.Now()
			s.Record(0, r, nil, now)
			s.Record(1, r, nil, now)
			s.Withdraw(0, now)
			s.Withdraw(1, now)
		}
	})
	for range 3000 {
		states, selected := s.statusSnapshot()
		for class, i := range selected {
			if i < 0 {
				continue
			}
			if i >= len(states) || !states[i].Healthy {
				t.Error("active path and health came from different observations")
				break
			}
			if class == 1 && i != 0 {
				t.Error("native UDP selected an ineligible carrier")
			}
			if probeDelay(s.policy, states[i].Healthy, true) != 5*time.Second {
				t.Error("selected path has standby cadence")
			}
		}
	}
	wg.Wait()
}
