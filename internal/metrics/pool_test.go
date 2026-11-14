package metrics

import (
	"encoding/json"
	"testing"
)

// The pool is allowed to outgrow its configured size, and from outside that is
// indistinguishable from a leak: somebody who set 8 and counts thirty sockets
// to their server cannot tell a tunnel under load from a broken one. These
// cover what has to reach the panel for that question to be answerable.

// A tunnel with no pool must report none, or every server tunnel and every
// datagram listener grows a "0 of 0" panel section for something they do not
// have.
func TestNoPoolIsReportedWhenThereIsNone(t *testing.T) {
	ClearPool()
	c := &Collector{}
	if s := c.Snapshot(); s.Pool != nil {
		t.Errorf("a pool was reported with none configured: %+v", s.Pool)
	}
}

// The configured size is what makes the live count readable, so it has to
// survive into the snapshot alongside it — a live count on its own is the
// number that looks like a leak.
func TestPoolReportsWhatItGrewFromAndWhy(t *testing.T) {
	ClearPool()
	t.Cleanup(ClearPool)

	ReportPool(19, 22, 8, 240)

	s := (&Collector{}).Snapshot()
	if s.Pool == nil {
		t.Fatal("a reported pool did not reach the snapshot")
	}
	if s.Pool.Live != 19 || s.Pool.Target != 22 || s.Pool.Configured != 8 {
		t.Errorf("pool = %+v, want live 19, target 22, configured 8", s.Pool)
	}
	if s.Pool.Mbps != 240 {
		t.Errorf("throughput = %d, want 240 — without it the growth has no explanation", s.Pool.Mbps)
	}
}

// Disconnecting clears it, so a restarted tunnel does not show the pool it had
// before it went down as though it were still open.
func TestClearingForgetsThePool(t *testing.T) {
	ReportPool(19, 22, 8, 240)
	ClearPool()

	if live, target, configured, mbps := PoolState(); live|target|configured|mbps != 0 {
		t.Errorf("stale pool state survived: live %d, target %d, configured %d, %d Mbit/s",
			live, target, configured, mbps)
	}
	if s := (&Collector{}).Snapshot(); s.Pool != nil {
		t.Errorf("a cleared pool still reached the snapshot: %+v", s.Pool)
	}
}

func TestWSMuxPoolDiagnosticsReachJSONAndClearOnDisconnect(t *testing.T) {
	ClearPool()
	t.Cleanup(ClearPool)
	ReportPool(12, 12, 8, 240)
	ReportMuxPool(&MuxPoolStats{Sessions: 12, ActiveSessions: 4, IdleSessions: 8, Streams: 500,
		PressureObserved: 10, PressuredSessions: 2, ThroughputGrowthSuppressed: true})
	s := (&Collector{}).Snapshot()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Pool == nil || decoded.Pool.Mux == nil || *decoded.Pool.Mux != *s.Pool.Mux {
		t.Fatalf("mux sample did not survive JSON: %s", data)
	}
	ClearPool()
	ReportPool(4, 4, 4, 0)
	if s := (&Collector{}).Snapshot(); s.Pool.Mux != nil {
		t.Fatal("old mux sample survived a disconnect into a different pool")
	}
}
