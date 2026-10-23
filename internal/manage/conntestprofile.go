package manage

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"
)

const (
	ConnTestQuick          = "quick"
	ConnTestFull           = "full"
	connTestQuickSamples   = 30
	connTestQuickFinalists = 3
	ctScreened             = "screened"
)

// Empty retains the full test for existing callers and older test links.
func ConnTestMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = ConnTestFull
	}
	if mode != ConnTestQuick && mode != ConnTestFull {
		return "", fmt.Errorf("test mode must be quick or full")
	}
	return mode, nil
}

func (c *connTestCase) samples() int {
	if c.mode == ConnTestQuick {
		return connTestQuickSamples
	}
	return connTestSoak
}

func ctEchoContext(ctx context.Context, conn net.Conn, udp bool) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	return ctEcho(conn, udp)
}

// Only fully completed, lossless traffic checks enter the speed shortlist.
// Sort by RTT plus twice jitter, then connect time; keep ties deterministic.
// Non-finalists retain their evidence without claiming a verified speed pass.
func ctShortlist(rows []ConnTestResult, limit int, report func(int, ConnTestResult)) {
	var candidates []int
	for i, r := range rows {
		if r.Phase == "speed-queued" && r.Status == ctTesting && r.Total > 0 && r.Tried == r.Total && r.OK == r.Total {
			candidates = append(candidates, i)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := rows[candidates[i]], rows[candidates[j]]
		if x, y := float64(a.RTTms)+2*a.JitterMs, float64(b.RTTms)+2*b.JitterMs; x != y {
			return x < y
		}
		return a.Connect < b.Connect
	})
	for rank, i := range candidates {
		if rank < limit {
			continue
		}
		r := rows[i]
		r.Status, r.Phase = ctScreened, ""
		r.Detail = "short traffic check passed; speed not measured (quick-test shortlist only)"
		report(i, r)
	}
}

// Estimates include the serial speed budget, not just the sampling window.
func ConnTestRunTimeFor(mode string) time.Duration {
	if mode == ConnTestQuick {
		return 30*time.Second + connTestQuickSamples*time.Second + 20*time.Second + ConnTestSpeedTime(connTestQuickFinalists)
	}
	return connTestConnectWait + time.Duration(connTestSoak)*time.Second + 45*time.Second + ConnTestSpeedTime(len(connTestReverse)-1+len(ctDirectPlans("")))
}
