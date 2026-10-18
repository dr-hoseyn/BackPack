package adaptive

import (
	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/pathprobe"
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
