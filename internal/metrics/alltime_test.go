package metrics

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Reported from the panel: "carried since this server was set up" went back
// down after every update. Three ways the figure lost traffic, each pinned here.

// The layer-3 engine counts its own traffic and hands the collector its
// counters. Those start from zero in every process, and they used to replace
// the carried-over total rather than add to it — so a direct tunnel's history
// was gone after every update, every restart, every config edit.
func TestAnEngineCountingItsOwnTrafficKeepsItsTotalAcrossARestart(t *testing.T) {
	resetCounters(t)
	dir := t.TempDir()

	var in, out atomic.Uint64
	first := NewCollector(dir, "pck", "l3-pck", "server", in.Load, out.Load)
	in.Store(7000)
	out.Store(3000)
	if err := first.Write(); err != nil {
		t.Fatal(err)
	}

	// The update: a new process, counters from zero.
	var in2, out2 atomic.Uint64
	second := NewCollector(dir, "pck", "l3-pck", "server", in2.Load, out2.Load)
	if got := second.Snapshot(); got.BytesIn != 7000 || got.BytesOut != 3000 {
		t.Fatalf("after a restart: in %d, out %d; want the previous 7000/3000", got.BytesIn, got.BytesOut)
	}
	in2.Store(100)
	out2.Store(50)
	if got := second.Snapshot(); got.BytesIn != 7100 || got.BytesOut != 3050 {
		t.Errorf("in %d, out %d; want 7100/3050 (previous plus new)", got.BytesIn, got.BytesOut)
	}
}

// A reload — a config edit, a restart asked for over the control socket —
// starts a new collector inside the same process. The process-wide counters
// are not reset by that, and the new collector's baseline already holds them:
// adding them again counted every byte of the process twice.
func TestAReloadInsideOneProcessDoesNotCountTwice(t *testing.T) {
	resetCounters(t)
	dir := t.TempDir()

	first := NewCollector(dir, "t", "tcp", "server", nil, nil)
	AddBytes(1000, 500)
	if err := first.Write(); err != nil {
		t.Fatal(err)
	}

	second := NewCollector(dir, "t", "tcp", "server", nil, nil)
	if got := second.Snapshot(); got.BytesIn != 1000 || got.BytesOut != 500 {
		t.Fatalf("after a reload: in %d, out %d; want 1000/500", got.BytesIn, got.BytesOut)
	}
	AddBytes(10, 5)
	if got := second.Snapshot(); got.BytesIn != 1010 || got.BytesOut != 505 {
		t.Errorf("in %d, out %d; want 1010/505", got.BytesIn, got.BytesOut)
	}
}

// Deleting a tunnel took everything it had carried out of the server's total,
// and a tunnel made again under the same name started from the old one's
// figure. What a deleted tunnel carried moves into the server's own ledger.
func TestADeletedTunnelsTrafficStaysInTheServerTotal(t *testing.T) {
	resetCounters(t)
	dir := t.TempDir()

	for _, name := range []string{"a", "b"} {
		bytesIn.Store(0)
		bytesOut.Store(0)
		c := NewCollector(dir, name, "tcp", "server", nil, nil)
		AddBytes(1000, 400)
		if err := c.Write(); err != nil {
			t.Fatal(err)
		}
	}
	if err := Retire(dir, "a"); err != nil {
		t.Fatal(err)
	}
	if err := Retire(dir, "b"); err != nil {
		t.Fatal(err)
	}
	if err := Retire(dir, "never-ran"); err != nil {
		t.Errorf("retiring a tunnel with no history: %v", err)
	}

	in, out := Retired(dir)
	if in != 2000 || out != 800 {
		t.Errorf("the ledger holds in %d, out %d; want 2000/800", in, out)
	}
	if _, err := Read(dir, "a"); err == nil {
		t.Error("the deleted tunnel's own file is still there; a new tunnel named a would inherit it")
	}

	bytesIn.Store(0)
	bytesOut.Store(0)
	again := NewCollector(dir, "a", "tcp", "server", nil, nil)
	if got := again.Snapshot(); got.BytesIn != 0 {
		t.Errorf("a new tunnel named like a deleted one started at %d", got.BytesIn)
	}
}

func TestRetiringATunnelPreservesAnUnreadableLedgerAndItsSnapshot(t *testing.T) {
	dir := t.TempDir()
	c := NewCollector(dir, "t1", "tcp", "server", nil, nil)
	if err := c.Write(); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(dir, retiredFile)
	const damaged = `{"bytes_in":`
	if err := os.WriteFile(ledger, []byte(damaged), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Retire(dir, "t1"); err == nil {
		t.Fatal("retirement succeeded with an unreadable ledger")
	}
	if b, err := os.ReadFile(ledger); err != nil || string(b) != damaged {
		t.Fatalf("ledger changed to %q, error %v", b, err)
	}
	if _, err := Read(dir, "t1"); err != nil {
		t.Fatalf("snapshot is missing after retirement failed: %v", err)
	}
}

func TestConcurrentRetirementsCountEachTunnelOnce(t *testing.T) {
	dir := t.TempDir()
	const tunnels = 12
	for i := range tunnels {
		name := fmt.Sprintf("t%d", i)
		if err := os.WriteFile(Path(dir, name), []byte(`{"bytes_in":100,"bytes_out":200}`), 0600); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	results := make(chan error, 2*tunnels)
	for i := range tunnels {
		name := fmt.Sprintf("t%d", i)
		for range 2 {
			go func() {
				<-start
				results <- Retire(dir, name)
			}()
		}
	}
	close(start)
	for range 2 * tunnels {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if in, out := Retired(dir); in != 100*tunnels || out != 200*tunnels {
		t.Fatalf("retired traffic is %d/%d, want %d/%d", in, out, 100*tunnels, 200*tunnels)
	}
}

func TestRetiringATunnelPreservesAnUnreadableSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "t1")
	const damaged = `{"bytes_in":`
	if err := os.WriteFile(path, []byte(damaged), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Retire(dir, "t1"); err == nil {
		t.Fatal("retirement discarded an unreadable snapshot")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != damaged {
		t.Fatalf("snapshot = %q, error %v", got, err)
	}
}

func TestRetirementRetryDoesNotCountACommittedSnapshotTwice(t *testing.T) {
	dir := t.TempDir()
	const name = "t1"
	body := []byte(`{"bytes_in":100,"bytes_out":200}`)
	if err := os.WriteFile(Path(dir, name), body, 0600); err != nil {
		t.Fatal(err)
	}
	r := retired{
		BytesIn:  100,
		BytesOut: 200,
		Pending:  map[string]string{name: fmt.Sprintf("%x", sha256.Sum256(body))},
	}
	if err := writeRetired(dir, r); err != nil {
		t.Fatal(err)
	}

	if err := Retire(dir, name); err != nil {
		t.Fatal(err)
	}
	r, err := readRetired(dir)
	if err != nil || r.BytesIn != 100 || r.BytesOut != 200 || len(r.Pending) != 0 {
		t.Fatalf("ledger after retry = %+v, error %v", r, err)
	}
	if _, err := os.Stat(Path(dir, name)); !os.IsNotExist(err) {
		t.Fatalf("snapshot still exists after retry: %v", err)
	}

	// A new tunnel with the same name can be retired independently.
	if err := os.WriteFile(Path(dir, name), body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Retire(dir, name); err != nil {
		t.Fatal(err)
	}
	if in, out := Retired(dir); in != 200 || out != 400 {
		t.Fatalf("totals after name reuse = %d/%d", in, out)
	}
}

func TestRetirementRetryPreservesAChangedSnapshot(t *testing.T) {
	dir := t.TempDir()
	const name = "t1"
	original := []byte(`{"bytes_in":100,"bytes_out":200}`)
	changed := []byte(`{"bytes_in":300,"bytes_out":400}`)
	if err := os.WriteFile(Path(dir, name), changed, 0600); err != nil {
		t.Fatal(err)
	}
	r := retired{
		BytesIn:  100,
		BytesOut: 200,
		Pending:  map[string]string{name: fmt.Sprintf("%x", sha256.Sum256(original))},
	}
	if err := writeRetired(dir, r); err != nil {
		t.Fatal(err)
	}
	if err := Retire(dir, name); err == nil {
		t.Fatal("retry accepted a different snapshot")
	}
	if got, err := os.ReadFile(Path(dir, name)); err != nil || string(got) != string(changed) {
		t.Fatalf("snapshot = %q, error %v", got, err)
	}
	if in, out := Retired(dir); in != 100 || out != 200 {
		t.Fatalf("ledger changed after rejected retry: %d/%d", in, out)
	}
}
