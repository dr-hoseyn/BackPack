package metrics

import (
	"os"
	"strings"
	"testing"
)

func resetCounters(t *testing.T) {
	t.Helper()
	bytesIn.Store(0)
	bytesOut.Store(0)
	t.Cleanup(func() { bytesIn.Store(0); bytesOut.Store(0) })
}

func TestHealthSnapshotsDoNotReadOrPersistTrafficTotals(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir, "t1"), []byte("unreadable old accounting"), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewCollector(dir, "t1", "tcp", "server", func() uint64 { t.Fatal("sampled accounting source"); return 0 }, nil)
	AddBytes(1024, 2048)
	if err := c.Write(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(Path(dir, "t1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "bytes_in") || strings.Contains(string(body), "bytes_out") {
		t.Fatalf("persisted byte counts: %s", body)
	}
	got, err := Read(dir, "t1")
	if err != nil || got.Name != "t1" || got.Transport != "tcp" {
		t.Fatalf("health snapshot=%+v, error %v", got, err)
	}
}
