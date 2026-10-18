//go:build !windows

package adaptive

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/backpack/backpack/config"
	"github.com/backpack/backpack/internal/pathprobe"
	"github.com/sirupsen/logrus"
)

func TestManagedHelperExitWithdrawsTCPAndUDPWithoutClosingActiveForwards(t *testing.T) {
	selector, err := NewSelector(config.PathPolicy{Failures: 10, MinDwell: 3600}, []bool{true, true})
	if err != nil {
		t.Fatal(err)
	}
	sample := pathprobe.Result{RTT: time.Millisecond, BytesPerSecond: 1e6}
	now := time.Now()
	selector.Record(0, sample, nil, now)
	selector.Record(1, sample, nil, now)
	idle, idlePeer := net.Pipe()
	defer idle.Close()
	defer idlePeer.Close()
	active, activePeer := net.Pipe()
	defer active.Close()
	defer activePeer.Close()
	log := logrus.New()
	log.SetOutput(io.Discard)
	c := &ClientPaths{selector: selector, log: log, paths: make([]clientPath, 2), connections: map[string]*proxyConnection{
		"idle":   {conn: idle, path: 0, idle: true},
		"active": {conn: active, path: 0},
	}}
	c.paths[0].name, c.paths[1].name = "primary", "backup"
	c.paths[1].ready.Store(true)
	c.paths[0].epoch.Add(1) // supervisor reported the primary helper's exit
	c.record(0, pathprobe.Result{}, nil)
	if selector.Pick(false) != 1 || selector.Pick(true) != 1 {
		t.Fatal("locally dead helper waited for remote failure threshold or dwell")
	}
	var b [1]byte
	idlePeer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := idlePeer.Read(b[:]); err != io.EOF {
		t.Fatalf("old idle pool connection survived: %v", err)
	}
	activePeer.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := activePeer.Read(b[:]); err == nil {
		t.Fatal("unexpected active data")
	} else if e, ok := err.(net.Error); !ok || !e.Timeout() {
		t.Fatalf("active forward was closed by selection change: %v", err)
	}
	c.paths[0].ready.Store(true)
	c.paths[0].epoch.Add(1)
	c.recordProbe(0, 0, sample, nil) // old process's late success must not revive it
	if selector.Status()[0].Healthy {
		t.Fatal("stale probe revived a restarted helper")
	}
	c.record(0, sample, nil)
	if selector.Status()[0].Healthy {
		t.Fatal("helper returned without the recovery threshold")
	}
	c.record(0, sample, nil)
	if !selector.Status()[0].Healthy {
		t.Fatal("verified recovered helper remained unavailable")
	}
	c.paths[1].ready.Store(false)
	c.record(1, pathprobe.Result{}, nil)
	if selector.Pick(false) != 0 || selector.Pick(true) != 0 {
		t.Fatal("verified primary was not used after backup exit")
	}
	c.paths[0].ready.Store(false)
	c.record(0, pathprobe.Result{}, nil)
	if selector.Pick(false) != -1 || selector.Pick(true) != -1 {
		t.Fatal("all-down state did not fail closed")
	}
}
